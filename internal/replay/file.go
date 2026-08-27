package replay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
)

type filePolicy uint8

const (
	stableFile filePolicy = iota
	appendOnlyFile
)

// readExactFile opens only rel beneath the configured provider root. It reads
// an immutable prefix into bounded memory and verifies identity and that prefix
// before returning. appendOnlyFile permits bytes appended after the first stat;
// it never incorporates them into the current Replay snapshot.
func (service *Service) readExactFile(
	ctx context.Context,
	profile Profile,
	provider ProviderID,
	rel string,
	policy filePolicy,
) ([]byte, error) {
	rootPath := service.dependencies.Roots[provider]
	if !providerRootAvailable(rootPath) {
		return nil, failure(CodeContainment, profile)
	}
	if service.beforeRootOpen != nil {
		service.beforeRootOpen(provider)
	}
	root, err := openPinnedRoot(rootPath)
	if err != nil {
		return nil, failure(CodeContainment, profile)
	}
	defer root.Close()

	file, err := root.Open(rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, failure(CodeHistoryMissing, profile)
		}
		return nil, failure(CodeContainment, profile)
	}
	defer file.Close()

	before, err := file.Stat()
	if err != nil {
		return nil, failure(CodeSourceFailure, profile)
	}
	if !before.Mode().IsRegular() {
		return nil, failure(CodeContainment, profile)
	}
	if before.Size() < 0 || before.Size() > service.limits.MaxSourceBytes {
		return nil, failure(CodeBoundExceeded, profile)
	}
	if before.Size() > int64(int(^uint(0)>>1)) {
		return nil, failure(CodeBoundExceeded, profile)
	}

	data := make([]byte, int(before.Size()))
	if err := readFullContext(ctx, file, data); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, failure(CodeCancelled, profile)
		}
		return nil, failure(CodeConcurrentWrite, profile)
	}
	initialDigest := sha256.Sum256(data)

	if service.afterFileRead != nil {
		service.afterFileRead(provider)
	}
	if err := ctx.Err(); err != nil {
		return nil, failure(CodeCancelled, profile)
	}

	if err := verifyFileSnapshot(file, root, rel, before, policy); err != nil {
		return nil, failure(CodeConcurrentWrite, profile)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, failure(CodeSourceFailure, profile)
	}
	hash := sha256.New()
	if err := copyNContext(ctx, hash, file, before.Size()); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, failure(CodeCancelled, profile)
		}
		return nil, failure(CodeConcurrentWrite, profile)
	}
	if !bytes.Equal(initialDigest[:], hash.Sum(nil)) {
		return nil, failure(CodeConcurrentWrite, profile)
	}
	if err := verifyFileSnapshot(file, root, rel, before, policy); err != nil {
		return nil, failure(CodeConcurrentWrite, profile)
	}
	return data, nil
}

func verifyFileSnapshot(file *os.File, root *os.Root, rel string, before os.FileInfo, policy filePolicy) error {
	descriptorInfo, err := file.Stat()
	if err != nil {
		return err
	}
	pathInfo, err := root.Stat(rel)
	if err != nil || !os.SameFile(before, descriptorInfo) || !os.SameFile(before, pathInfo) {
		return errors.New("file identity changed")
	}
	if descriptorInfo.Size() < before.Size() || pathInfo.Size() < before.Size() {
		return errors.New("file rewound")
	}
	if policy == stableFile &&
		(descriptorInfo.Size() != before.Size() || pathInfo.Size() != before.Size() ||
			!descriptorInfo.ModTime().Equal(before.ModTime()) || !pathInfo.ModTime().Equal(before.ModTime())) {
		return errors.New("stable file changed")
	}
	return nil
}

func readFullContext(ctx context.Context, reader io.Reader, destination []byte) error {
	for offset := 0; offset < len(destination); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(offset+(64<<10), len(destination))
		count, err := io.ReadFull(reader, destination[offset:end])
		offset += count
		if err != nil {
			return err
		}
	}
	return nil
}

func copyNContext(ctx context.Context, writer io.Writer, reader io.Reader, size int64) error {
	buffer := make([]byte, 64<<10)
	for remaining := size; remaining > 0; {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := min(int64(len(buffer)), remaining)
		count, err := io.ReadFull(reader, buffer[:chunk])
		if count > 0 {
			if _, writeErr := writer.Write(buffer[:count]); writeErr != nil {
				return writeErr
			}
			remaining -= int64(count)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// jsonLines returns complete, non-empty JSONL records without copying their
// bytes. A permitted unterminated tail is excluded as a live partial write.
func (service *Service) jsonLines(data []byte, allowPartialTail bool, profile Profile) ([][]byte, error) {
	lines := make([][]byte, 0, min(1024, service.limits.MaxRecords))
	start := 0
	for start < len(data) {
		relativeEnd := bytes.IndexByte(data[start:], '\n')
		if relativeEnd < 0 {
			if len(data)-start > service.limits.MaxRecordBytes {
				return nil, failure(CodeBoundExceeded, profile)
			}
			if !allowPartialTail {
				return nil, failure(CodeCorruptHistory, profile)
			}
			break
		}
		end := start + relativeEnd
		line := data[start:end]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if len(bytes.TrimSpace(line)) == 0 {
			return nil, failure(CodeCorruptHistory, profile)
		}
		if len(line) > service.limits.MaxRecordBytes {
			return nil, failure(CodeBoundExceeded, profile)
		}
		if len(lines) == service.limits.MaxRecords {
			return nil, failure(CodeBoundExceeded, profile)
		}
		lines = append(lines, line)
		start = end + 1
	}
	return lines, nil
}
