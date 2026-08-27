package connector

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/replay"
)

const (
	MaxReplayTargets          = 16
	MaxReplayCount            = 32
	MaxReplayInvocations      = MaxReplayCount
	MaxReplayRecordsPerTarget = 1024
	MaxReplayTargetBytes      = MaxCapabilityResultBytes
)

type ownedReplayTarget struct {
	childID   string
	provider  string
	reference replay.OpaqueRef
}

type replayResultTarget struct {
	ChildSessionID string
	Provider       replay.ProviderID
	Profile        replay.ProfileID
	Invocations    []replayResultInvocation
}

type replayResultInvocation struct {
	InvocationID string
	Status       string
	Records      []json.RawMessage
}

func (d *Delegator) replay(ctx context.Context, input delegateInput) (capabilityResult, error) {
	owned := make([]ownedReplayTarget, 0, len(input.ReplayTargets))
	reserved := make([]*childState, 0, len(input.ReplayTargets))
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return capabilityResult{}, ErrTransport
	}
	for _, childID := range input.ReplayTargets {
		child, ok := d.children[childID]
		if !ok || child.closed {
			clearReplayReservations(reserved)
			d.mu.Unlock()
			return capabilityResult{}, ErrOwnership
		}
		// Transient counters cannot prove completion after a runtime restart.
		// Only an actively running child is ineligible here; the exact native
		// Replay adapter is authoritative for completed-range availability.
		if child.active != nil || child.resuming || child.replaying {
			clearReplayReservations(reserved)
			d.mu.Unlock()
			return capabilityResult{}, ErrReplayUnavailable
		}
		child.replaying = true
		reserved = append(reserved, child)
		owned = append(owned, ownedReplayTarget{
			childID: child.id, provider: child.provider,
			reference: replay.OpaqueRefFromBytes(child.reference.Bytes()),
		})
	}
	d.mu.Unlock()
	defer d.releaseReplayReservations(reserved)

	collector := &replayCollector{}
	for _, target := range owned {
		profile, err := d.registry.Profile(target.provider)
		if err != nil || !profile.Capabilities.Replay || !profile.Replay.available(profile.conformance) {
			collector.clear()
			return capabilityResult{}, ErrProviderUnsupported
		}
		nativeProfile, ok := profile.Replay.Port.Capability(profile.Replay.Provider, profile.Replay.InstalledVersion)
		if !ok {
			collector.clear()
			return capabilityResult{}, ErrProviderUnsupported
		}
		collector.expect(target.childID, profile.Replay.Provider, nativeProfile.ID, input.ReplayCount)
		err = profile.Replay.Port.Replay(ctx, replay.BatchRequest{Targets: []replay.Request{{
			TargetID:         target.childID,
			Provider:         profile.Replay.Provider,
			InstalledVersion: profile.Replay.InstalledVersion,
			Reference:        replay.OpaqueRefFromBytes(target.reference.Bytes()),
			Count:            input.ReplayCount,
		}}}, collector)
		if err != nil {
			failure := collector.failure
			collector.clear()
			if failure != nil {
				return capabilityResult{}, failure
			}
			return capabilityResult{}, replayBoundaryError(ctx, err)
		}
		if collector.failure != nil || collector.currentTarget != nil || collector.currentInvocation != nil {
			failure := collector.failure
			collector.clear()
			if failure == nil {
				failure = ErrReplayUnavailable
			}
			return capabilityResult{}, failure
		}
	}

	result := capabilityResult{Operation: OperationReplay, ReplayTargets: collector.targets, More: false}
	if _, err := result.marshal(); err != nil {
		collector.clear()
		return capabilityResult{}, err
	}
	return result, nil
}

// clearReplayReservations is called only while d.mu is held.
func clearReplayReservations(children []*childState) {
	for _, child := range children {
		child.replaying = false
	}
}

func (d *Delegator) releaseReplayReservations(children []*childState) {
	d.mu.Lock()
	clearReplayReservations(children)
	d.mu.Unlock()
}

func replayBoundaryError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	code, ok := replay.CodeOf(err)
	if !ok {
		return &BoundaryError{Stage: "replay", Err: ErrReplayUnavailable}
	}
	switch code {
	case replay.CodeUnsupportedVersion, replay.CodeUnknownSchema, replay.CodeSourceFailure:
		return &BoundaryError{Stage: "replay", Err: ErrProviderUnsupported}
	case replay.CodeBoundExceeded, replay.CodeSinkFailure:
		return &BoundaryError{Stage: "replay", Err: ErrCapacity}
	default:
		return &BoundaryError{Stage: "replay", Err: ErrReplayUnavailable}
	}
}

type replayCollector struct {
	expectedTarget   string
	expectedProvider replay.ProviderID
	expectedProfile  replay.ProfileID
	expectedCount    uint32

	targets           []replayResultTarget
	currentTarget     *replayResultTarget
	currentInvocation *replayResultInvocation
	targetBytes       int
	targetRecords     int
	failure           error
}

func (collector *replayCollector) expect(target string, provider replay.ProviderID, profile replay.ProfileID, count uint32) {
	collector.expectedTarget = target
	collector.expectedProvider = provider
	collector.expectedProfile = profile
	collector.expectedCount = count
}

func (collector *replayCollector) BeginTarget(boundary replay.TargetBoundary) error {
	if collector.failure != nil {
		return collector.failure
	}
	if collector.currentTarget != nil || collector.currentInvocation != nil || boundary.TargetID != collector.expectedTarget ||
		boundary.Provider != collector.expectedProvider || boundary.Profile != collector.expectedProfile {
		return collector.fail(ErrReplayUnavailable)
	}
	collector.currentTarget = &replayResultTarget{
		ChildSessionID: boundary.TargetID,
		Provider:       boundary.Provider,
		Profile:        boundary.Profile,
	}
	collector.targetBytes = 0
	collector.targetRecords = 0
	return nil
}

func (collector *replayCollector) BeginInvocation(boundary replay.NativeBoundary) error {
	if collector.failure != nil {
		return collector.failure
	}
	if collector.currentTarget == nil || collector.currentInvocation != nil || boundary.InvocationID == "" ||
		len(boundary.InvocationID) > MaxCorrelationIDBytes || boundary.Status == "" || len(boundary.Status) > MaxCorrelationIDBytes ||
		len(collector.currentTarget.Invocations) >= MaxReplayInvocations {
		return collector.fail(ErrReplayUnavailable)
	}
	collector.currentInvocation = &replayResultInvocation{
		InvocationID: boundary.InvocationID,
		Status:       boundary.Status,
	}
	return nil
}

func (collector *replayCollector) Record(record replay.NativeRecord) error {
	if collector.failure != nil {
		return collector.failure
	}
	if collector.currentInvocation == nil || len(record.JSON) == 0 || len(record.JSON) > acp.MaxFrameBytes || !json.Valid(record.JSON) {
		return collector.fail(ErrReplayUnavailable)
	}
	if collector.targetRecords >= MaxReplayRecordsPerTarget || collector.targetBytes+len(record.JSON) > MaxReplayTargetBytes {
		return collector.fail(ErrCapacity)
	}
	collector.currentInvocation.Records = append(
		collector.currentInvocation.Records,
		append(json.RawMessage(nil), record.JSON...),
	)
	collector.targetRecords++
	collector.targetBytes += len(record.JSON)
	return nil
}

func (collector *replayCollector) EndInvocation(boundary replay.NativeBoundary) error {
	if collector.failure != nil {
		return collector.failure
	}
	if collector.currentTarget == nil || collector.currentInvocation == nil ||
		collector.currentInvocation.InvocationID != boundary.InvocationID || collector.currentInvocation.Status != boundary.Status {
		return collector.fail(ErrReplayUnavailable)
	}
	collector.currentTarget.Invocations = append(collector.currentTarget.Invocations, *collector.currentInvocation)
	collector.currentInvocation = nil
	return nil
}

func (collector *replayCollector) EndTarget(boundary replay.TargetBoundary) error {
	if collector.failure != nil {
		return collector.failure
	}
	if collector.currentTarget == nil || collector.currentInvocation != nil ||
		uint32(len(collector.currentTarget.Invocations)) != collector.expectedCount ||
		collector.currentTarget.ChildSessionID != boundary.TargetID || collector.currentTarget.Provider != boundary.Provider ||
		collector.currentTarget.Profile != boundary.Profile {
		return collector.fail(ErrReplayUnavailable)
	}
	collector.targets = append(collector.targets, *collector.currentTarget)
	collector.currentTarget = nil
	return nil
}

func (collector *replayCollector) fail(err error) error {
	if collector.failure == nil {
		collector.failure = err
	}
	return collector.failure
}

func (collector *replayCollector) clear() {
	clearReplayTargets(collector.targets)
	if collector.currentTarget != nil {
		clearReplayTargets([]replayResultTarget{*collector.currentTarget})
	}
	if collector.currentInvocation != nil {
		for index := range collector.currentInvocation.Records {
			collector.currentInvocation.Records[index] = nil
		}
	}
	*collector = replayCollector{}
}

func clearReplayTargets(targets []replayResultTarget) {
	for targetIndex := range targets {
		for invocationIndex := range targets[targetIndex].Invocations {
			for recordIndex := range targets[targetIndex].Invocations[invocationIndex].Records {
				targets[targetIndex].Invocations[invocationIndex].Records[recordIndex] = nil
			}
		}
	}
}

func marshalReplayTargets(targets []replayResultTarget) ([]byte, error) {
	if len(targets) == 0 || len(targets) > MaxReplayTargets {
		return nil, ErrReplayUnavailable
	}
	var out bytes.Buffer
	out.WriteByte('[')
	for targetIndex, target := range targets {
		if targetIndex != 0 {
			out.WriteByte(',')
		}
		if target.ChildSessionID == "" || target.Provider == "" || target.Profile == "" || len(target.Invocations) == 0 {
			return nil, ErrReplayUnavailable
		}
		childID, _ := json.Marshal(target.ChildSessionID)
		provider, _ := json.Marshal(target.Provider)
		profile, _ := json.Marshal(target.Profile)
		out.WriteString(`{"child_session_id":`)
		out.Write(childID)
		out.WriteString(`,"provider":`)
		out.Write(provider)
		out.WriteString(`,"profile":`)
		out.Write(profile)
		out.WriteString(`,"invocations":[`)
		for invocationIndex, invocation := range target.Invocations {
			if invocationIndex != 0 {
				out.WriteByte(',')
			}
			invocationID, _ := json.Marshal(invocation.InvocationID)
			status, _ := json.Marshal(invocation.Status)
			out.WriteString(`{"invocation_id":`)
			out.Write(invocationID)
			out.WriteString(`,"status":`)
			out.Write(status)
			out.WriteString(`,"records":[`)
			for recordIndex, record := range invocation.Records {
				if len(record) == 0 || !json.Valid(record) {
					return nil, ErrReplayUnavailable
				}
				if recordIndex != 0 {
					out.WriteByte(',')
				}
				out.Write(record)
			}
			out.WriteString(`]}`)
		}
		out.WriteString(`]}`)
	}
	out.WriteByte(']')
	if !json.Valid(out.Bytes()) {
		return nil, ErrReplayUnavailable
	}
	return out.Bytes(), nil
}

var _ replay.Sink = (*replayCollector)(nil)
var _ ReplayPort = (*replay.Service)(nil)
