package replay

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const maxTargetIDBytes = 256

type invocation struct {
	boundary NativeBoundary
	records  []json.RawMessage
	index    *int
}

type preparedTarget struct {
	boundary    TargetBoundary
	invocations []invocation
	bytes       int64
}

func (target *preparedTarget) close() {
	clearInvocations(target.invocations)
	target.invocations = nil
	target.bytes = 0
}

func clearInvocations(invocations []invocation) {
	for index := range invocations {
		clearInvocation(&invocations[index])
	}
}

func clearInvocation(item *invocation) {
	for recordIndex := range item.records {
		item.records[recordIndex] = nil
	}
	*item = invocation{}
}

// Service validates and streams exact provider-native Replay ranges.
type Service struct {
	dependencies Dependencies
	limits       Limits
	replayGate   chan struct{}

	// afterFileRead is an unexported deterministic concurrency test seam. It is
	// nil in production and receives no provider content or opaque reference.
	afterFileRead func(ProviderID)
	// beforeRootOpen and beforeGateWait are also content-free deterministic
	// seams used only to prove root-swap rejection and aggregate serialization.
	beforeRootOpen func(ProviderID)
	beforeGateWait func()
}

// NewService constructs a bounded Replay service without touching provider state.
func NewService(dependencies Dependencies, limits Limits) (*Service, error) {
	normalized, err := normalizeLimits(limits)
	if err != nil {
		return nil, ErrBoundExceeded
	}
	roots := make(map[ProviderID]string, len(dependencies.Roots))
	for provider, root := range dependencies.Roots {
		roots[provider] = root
	}
	dependencies.Roots = roots
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return &Service{dependencies: dependencies, limits: normalized, replayGate: gate}, nil
}

func normalizeLimits(limits Limits) (Limits, error) {
	if limits.MaxTargets == 0 {
		limits.MaxTargets = DefaultLimits.MaxTargets
	}
	if limits.MaxCount == 0 {
		limits.MaxCount = DefaultLimits.MaxCount
	}
	if limits.MaxSourceBytes == 0 {
		limits.MaxSourceBytes = DefaultLimits.MaxSourceBytes
	}
	if limits.MaxRecordBytes == 0 {
		limits.MaxRecordBytes = DefaultLimits.MaxRecordBytes
	}
	if limits.MaxRecords == 0 {
		limits.MaxRecords = DefaultLimits.MaxRecords
	}
	if limits.MaxSelectedBytes == 0 {
		limits.MaxSelectedBytes = DefaultLimits.MaxSelectedBytes
	}
	if limits.MaxBatchBytes == 0 {
		limits.MaxBatchBytes = DefaultLimits.MaxBatchBytes
	}
	if limits.MaxTargets < 1 || limits.MaxCount < 1 || limits.MaxSourceBytes < 1 ||
		limits.MaxRecordBytes < 1 || limits.MaxRecords < 1 || limits.MaxSelectedBytes < 1 ||
		limits.MaxBatchBytes < 1 {
		return Limits{}, errors.New("invalid replay limits")
	}
	return limits, nil
}

// Capability reports only compile-time profile and dependency availability.
// Per-Session structural conformance is still checked by Replay.
func (service *Service) Capability(provider ProviderID, installedVersion string) (Profile, bool) {
	profile, ok := ConformedProfile(provider, installedVersion)
	if !ok {
		return Profile{}, false
	}
	if provider == ProviderCodex {
		return profile, service.dependencies.Codex != nil
	}
	root := service.dependencies.Roots[provider]
	return profile, providerRootAvailable(root)
}

func providerRootAvailable(root string) bool {
	if !rootPinningSupported() || root == "" || !filepath.IsAbs(root) || strings.ContainsRune(root, 0) {
		return false
	}
	info, err := os.Lstat(root)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

// Replay prepares every target before emitting any native record. Exactness or
// availability failures therefore have zero Sink side effects.
func (service *Service) Replay(ctx context.Context, request BatchRequest, sink Sink) error {
	if err := ctx.Err(); err != nil {
		return &Error{Code: CodeCancelled}
	}
	if service.beforeGateWait != nil {
		service.beforeGateWait()
	}
	select {
	case <-ctx.Done():
		return &Error{Code: CodeCancelled}
	case <-service.replayGate:
	}
	defer func() { service.replayGate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return &Error{Code: CodeCancelled}
	}
	if sink == nil || len(request.Targets) == 0 || len(request.Targets) > service.limits.MaxTargets {
		return &Error{Code: CodeBoundExceeded}
	}

	prepared := make([]*preparedTarget, 0, len(request.Targets))
	defer func() {
		for _, target := range prepared {
			target.close()
		}
	}()

	seenTargetIDs := make(map[string]struct{}, len(request.Targets))
	var preparedBytes int64
	for _, targetRequest := range request.Targets {
		if err := ctx.Err(); err != nil {
			return &Error{Code: CodeCancelled}
		}
		if targetRequest.TargetID == "" || len(targetRequest.TargetID) > maxTargetIDBytes ||
			targetRequest.Count == 0 || targetRequest.Count > service.limits.MaxCount {
			return &Error{Code: CodeBoundExceeded, Provider: targetRequest.Provider}
		}
		if _, duplicate := seenTargetIDs[targetRequest.TargetID]; duplicate {
			return &Error{Code: CodeInvalidReference, Provider: targetRequest.Provider}
		}
		seenTargetIDs[targetRequest.TargetID] = struct{}{}

		profile, ok := ConformedProfile(targetRequest.Provider, targetRequest.InstalledVersion)
		if !ok {
			return &Error{Code: CodeUnsupportedVersion, Provider: targetRequest.Provider}
		}
		if _, available := service.Capability(targetRequest.Provider, targetRequest.InstalledVersion); !available {
			return failure(CodeSourceFailure, profile)
		}
		target, err := service.prepare(ctx, profile, targetRequest)
		if err != nil {
			return err
		}
		if target.bytes > service.limits.MaxBatchBytes-preparedBytes {
			target.close()
			return failure(CodeBoundExceeded, profile)
		}
		preparedBytes += target.bytes
		prepared = append(prepared, target)
	}

	for _, target := range prepared {
		if err := ctx.Err(); err != nil {
			return &Error{Code: CodeCancelled, Provider: target.boundary.Provider, Profile: target.boundary.Profile}
		}
		if err := sink.BeginTarget(target.boundary); err != nil {
			return &Error{Code: CodeSinkFailure, Provider: target.boundary.Provider, Profile: target.boundary.Profile}
		}
		for _, item := range target.invocations {
			if err := ctx.Err(); err != nil {
				return &Error{Code: CodeCancelled, Provider: target.boundary.Provider, Profile: target.boundary.Profile}
			}
			if err := sink.BeginInvocation(item.boundary); err != nil {
				return &Error{Code: CodeSinkFailure, Provider: target.boundary.Provider, Profile: target.boundary.Profile}
			}
			for _, raw := range item.records {
				if err := ctx.Err(); err != nil {
					return &Error{Code: CodeCancelled, Provider: target.boundary.Provider, Profile: target.boundary.Profile}
				}
				if err := sink.Record(NativeRecord{JSON: raw}); err != nil {
					return &Error{Code: CodeSinkFailure, Provider: target.boundary.Provider, Profile: target.boundary.Profile}
				}
			}
			if err := ctx.Err(); err != nil {
				return &Error{Code: CodeCancelled, Provider: target.boundary.Provider, Profile: target.boundary.Profile}
			}
			if err := sink.EndInvocation(item.boundary); err != nil {
				return &Error{Code: CodeSinkFailure, Provider: target.boundary.Provider, Profile: target.boundary.Profile}
			}
		}
		if err := ctx.Err(); err != nil {
			return &Error{Code: CodeCancelled, Provider: target.boundary.Provider, Profile: target.boundary.Profile}
		}
		if err := sink.EndTarget(target.boundary); err != nil {
			return &Error{Code: CodeSinkFailure, Provider: target.boundary.Provider, Profile: target.boundary.Profile}
		}
	}
	return nil
}

func (service *Service) prepare(ctx context.Context, profile Profile, request Request) (*preparedTarget, error) {
	reference, err := decodeReference(request.Reference, request.Provider, profile)
	if err != nil {
		return nil, err
	}

	var completed []invocation
	switch profile.ID {
	case ProfileCodex01501:
		completed, err = service.prepareCodex(ctx, profile, reference)
	case ProfileClaude21226:
		completed, err = service.prepareClaude(ctx, profile, reference)
	case ProfileGemini0250:
		completed, err = service.prepareGemini(ctx, profile, reference)
	case ProfileGrok105:
		completed, err = service.prepareGrok(ctx, profile, reference)
	default:
		err = failure(CodeUnsupportedVersion, profile)
	}
	if err != nil {
		return nil, err
	}
	if uint32(len(completed)) < request.Count {
		clearInvocations(completed)
		return nil, failure(CodeRangeUnavailable, profile)
	}
	chosen := completed[len(completed)-int(request.Count):]
	selected := make([]invocation, 0, len(chosen))
	var selectedBytes int64
	var retainedBytes int64 = int64(128 + len(request.TargetID) + len(profile.ID) + len(profile.Provider))
	for _, item := range chosen {
		if err := ctx.Err(); err != nil {
			clearInvocations(selected)
			clearInvocations(completed)
			return nil, failure(CodeCancelled, profile)
		}
		selectedItem := invocation{
			boundary: NativeBoundary{
				InvocationID: strings.Clone(item.boundary.InvocationID),
				Status:       strings.Clone(item.boundary.Status),
			},
			records: make([]json.RawMessage, 0, len(item.records)),
		}
		if item.index != nil {
			index := *item.index
			selectedItem.index = &index
		}
		retainedBytes += int64(128 + len(selectedItem.boundary.InvocationID) + len(selectedItem.boundary.Status))
		for _, record := range item.records {
			if err := ctx.Err(); err != nil {
				clearInvocation(&selectedItem)
				clearInvocations(selected)
				clearInvocations(completed)
				return nil, failure(CodeCancelled, profile)
			}
			selectedBytes += int64(len(record))
			if selectedBytes > service.limits.MaxSelectedBytes {
				clearInvocation(&selectedItem)
				clearInvocations(selected)
				clearInvocations(completed)
				return nil, failure(CodeBoundExceeded, profile)
			}
			selectedItem.records = append(selectedItem.records, append(json.RawMessage(nil), record...))
			retainedBytes += int64(32 + len(record))
		}
		selected = append(selected, selectedItem)
	}
	clearInvocations(completed)

	return &preparedTarget{
		boundary: TargetBoundary{
			TargetID: request.TargetID,
			Provider: request.Provider,
			Profile:  profile.ID,
		},
		invocations: selected,
		bytes:       retainedBytes,
	}, nil
}
