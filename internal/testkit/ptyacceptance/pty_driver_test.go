//go:build acceptance && (darwin || linux)

package ptyacceptance

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

const acceptanceWait = 12 * time.Second

type terminalSize struct {
	columns uint16
	rows    uint16
}

type viewerProcess struct {
	command *exec.Cmd
	pty     *os.File
	capture *terminalCapture
	result  *processResult
}

type processResult struct {
	done chan struct{}

	mu  sync.Mutex
	err error
}

func newProcessResult() *processResult { return &processResult{done: make(chan struct{})} }

func (result *processResult) finish(err error) {
	result.mu.Lock()
	result.err = err
	result.mu.Unlock()
	close(result.done)
}

func (result *processResult) wait(timeout time.Duration) (error, bool) {
	select {
	case <-result.done:
		result.mu.Lock()
		defer result.mu.Unlock()
		return result.err, true
	case <-time.After(timeout):
		return nil, false
	}
}

func (result *processResult) error() error {
	result.mu.Lock()
	defer result.mu.Unlock()
	return result.err
}

func startViewer(
	t *testing.T,
	binary, project string,
	environment []string,
	size terminalSize,
) *viewerProcess {
	t.Helper()
	command := exec.Command(binary)
	command.Dir = project
	command.Env = append([]string(nil), environment...)
	master, err := pty.StartWithSize(command, &pty.Winsize{Rows: size.rows, Cols: size.columns})
	if err != nil {
		t.Fatalf("start taskai viewer at %dx%d: %v", size.columns, size.rows, err)
	}
	capture := newTerminalCapture(int(size.columns), int(size.rows))
	viewer := &viewerProcess{command: command, pty: master, capture: capture, result: newProcessResult()}
	go capture.read(master)
	go func() { viewer.result.finish(command.Wait()) }()
	t.Cleanup(func() { viewer.terminate(t) })
	return viewer
}

func (viewer *viewerProcess) send(t *testing.T, value string) {
	t.Helper()
	if viewer == nil || viewer.pty == nil {
		t.Fatal("viewer is unavailable")
	}
	for len(value) > 0 {
		written, err := viewer.pty.Write([]byte(value))
		if err != nil || written <= 0 || written > len(value) {
			t.Fatalf("write viewer input: written=%d error=%v", written, err)
		}
		value = value[written:]
	}
}

func (viewer *viewerProcess) resize(t *testing.T, size terminalSize) {
	t.Helper()
	if err := pty.Setsize(viewer.pty, &pty.Winsize{Rows: size.rows, Cols: size.columns}); err != nil {
		t.Fatalf("resize viewer: %v", err)
	}
	viewer.capture.resize(int(size.columns), int(size.rows))
}

func (viewer *viewerProcess) waitContains(t *testing.T, marker string) {
	t.Helper()
	if err := viewer.capture.waitContains(marker, acceptanceWait, viewer.result); err != nil {
		t.Fatalf("wait for terminal marker %q: %v\nvisible:\n%s\ntranscript tail:\n%s",
			marker, err, viewer.capture.visible(), viewer.capture.tail(6000))
	}
}

func (viewer *viewerProcess) waitExit(t *testing.T) {
	t.Helper()
	if err, completed := viewer.result.wait(acceptanceWait); completed {
		_ = viewer.pty.Close()
		if err != nil {
			t.Fatalf("taskai viewer exited unsuccessfully: %v\n%s", err, viewer.capture.tail(6000))
		}
		viewer.command = nil
	} else {
		t.Fatalf("taskai viewer did not exit\n%s", viewer.capture.tail(6000))
	}
}

func (viewer *viewerProcess) quitFromTasks(t *testing.T) {
	t.Helper()
	viewer.waitContains(t, "TaskAI / Tasks")
	viewer.send(t, "q")
	viewer.waitExit(t)
	viewer.pty = nil
	viewer.capture = nil
}

func (viewer *viewerProcess) terminate(t *testing.T) {
	t.Helper()
	if viewer == nil || viewer.command == nil || viewer.command.Process == nil {
		return
	}
	_ = viewer.command.Process.Signal(syscall.SIGTERM)
	if _, completed := viewer.result.wait(2 * time.Second); !completed {
		_ = viewer.command.Process.Kill()
		if _, completed := viewer.result.wait(2 * time.Second); !completed {
			t.Errorf("viewer process %d could not be reaped", viewer.command.Process.Pid)
		}
	}
	if viewer.pty != nil {
		_ = viewer.pty.Close()
	}
	viewer.command = nil
}

type terminalCapture struct {
	mu sync.Mutex

	width  int
	height int
	cells  [][]byte
	row    int
	column int
	savedR int
	savedC int

	state       escapeState
	csi         []byte
	oscSawESC   bool
	transcript  []byte
	changed     chan struct{}
	closed      bool
	terminalErr error
}

type escapeState uint8

const (
	escapeNormal escapeState = iota
	escapeStarted
	escapeCSI
	escapeOSC
)

func newTerminalCapture(width, height int) *terminalCapture {
	capture := &terminalCapture{changed: make(chan struct{})}
	capture.resizeLocked(width, height)
	return capture
}

func (capture *terminalCapture) read(reader io.Reader) {
	buffer := make([]byte, 32<<10)
	for {
		read, err := reader.Read(buffer)
		if read > 0 {
			capture.feed(buffer[:read])
		}
		if err != nil {
			capture.mu.Lock()
			capture.closed = true
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) && !errors.Is(err, syscall.EIO) {
				capture.terminalErr = err
			}
			capture.notifyLocked()
			capture.mu.Unlock()
			return
		}
	}
}

func (capture *terminalCapture) feed(raw []byte) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	for _, value := range raw {
		capture.consume(value)
	}
	const maxTranscript = 2 << 20
	if len(capture.transcript) > maxTranscript {
		copy(capture.transcript, capture.transcript[len(capture.transcript)-maxTranscript:])
		capture.transcript = capture.transcript[:maxTranscript]
	}
	capture.notifyLocked()
}

func (capture *terminalCapture) consume(value byte) {
	switch capture.state {
	case escapeNormal:
		capture.consumeNormal(value)
	case escapeStarted:
		switch value {
		case '[':
			capture.state = escapeCSI
			capture.csi = capture.csi[:0]
		case ']':
			capture.state = escapeOSC
			capture.oscSawESC = false
		default:
			capture.state = escapeNormal
		}
	case escapeCSI:
		capture.csi = append(capture.csi, value)
		if value >= 0x40 && value <= 0x7e {
			capture.applyCSI(capture.csi)
			capture.csi = capture.csi[:0]
			capture.state = escapeNormal
		}
	case escapeOSC:
		if value == '\a' || (capture.oscSawESC && value == '\\') {
			capture.state = escapeNormal
			capture.oscSawESC = false
			return
		}
		capture.oscSawESC = value == 0x1b
	}
}

func (capture *terminalCapture) consumeNormal(value byte) {
	switch value {
	case 0x1b:
		capture.state = escapeStarted
	case '\r':
		capture.column = 0
		capture.appendTranscript('\n')
	case '\n':
		capture.row++
		if capture.row >= capture.height {
			capture.scroll()
			capture.row = capture.height - 1
		}
		capture.appendTranscript('\n')
	case '\t':
		spaces := 8 - capture.column%8
		for range spaces {
			capture.put(' ')
		}
	default:
		if value >= 0x20 && value < 0x7f {
			capture.put(value)
			capture.appendTranscript(value)
		}
	}
}

func (capture *terminalCapture) put(value byte) {
	if capture.height == 0 || capture.width == 0 {
		return
	}
	if capture.column >= capture.width {
		capture.column = 0
		capture.row++
	}
	if capture.row >= capture.height {
		capture.scroll()
		capture.row = capture.height - 1
	}
	if capture.row < 0 {
		capture.row = 0
	}
	capture.cells[capture.row][capture.column] = value
	capture.column++
}

func (capture *terminalCapture) scroll() {
	if capture.height < 1 {
		return
	}
	copy(capture.cells, capture.cells[1:])
	capture.cells[capture.height-1] = bytes.Repeat([]byte{' '}, capture.width)
}

func (capture *terminalCapture) applyCSI(sequence []byte) {
	if len(sequence) == 0 {
		return
	}
	final := sequence[len(sequence)-1]
	rawParams := strings.TrimLeft(string(sequence[:len(sequence)-1]), "?<>=!")
	params := parseCSIParams(rawParams)
	parameter := func(index, fallback int) int {
		if index >= len(params) || params[index] == 0 {
			return fallback
		}
		return params[index]
	}
	switch final {
	case 'H', 'f':
		capture.row = clampInt(parameter(0, 1)-1, 0, capture.height-1)
		capture.column = clampInt(parameter(1, 1)-1, 0, capture.width-1)
	case 'A':
		capture.row = clampInt(capture.row-parameter(0, 1), 0, capture.height-1)
	case 'B':
		capture.row = clampInt(capture.row+parameter(0, 1), 0, capture.height-1)
	case 'C':
		capture.column = clampInt(capture.column+parameter(0, 1), 0, capture.width-1)
	case 'D':
		capture.column = clampInt(capture.column-parameter(0, 1), 0, capture.width-1)
	case 'E':
		capture.row = clampInt(capture.row+parameter(0, 1), 0, capture.height-1)
		capture.column = 0
	case 'F':
		capture.row = clampInt(capture.row-parameter(0, 1), 0, capture.height-1)
		capture.column = 0
	case 'G':
		capture.column = clampInt(parameter(0, 1)-1, 0, capture.width-1)
	case 'd':
		capture.row = clampInt(parameter(0, 1)-1, 0, capture.height-1)
	case 'J':
		if len(params) == 0 || params[0] == 0 || params[0] == 2 || params[0] == 3 {
			capture.clear()
		}
	case 'K':
		capture.eraseLine(params)
	case 's':
		capture.savedR, capture.savedC = capture.row, capture.column
	case 'u':
		capture.row, capture.column = capture.savedR, capture.savedC
	case 'h':
		if strings.Contains(rawParams, "1049") {
			capture.clear()
		}
	}
}

func parseCSIParams(raw string) []int {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ";")
	values := make([]int, len(parts))
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err == nil {
			values[index] = value
		}
	}
	return values
}

func (capture *terminalCapture) eraseLine(params []int) {
	if capture.row < 0 || capture.row >= capture.height {
		return
	}
	mode := 0
	if len(params) > 0 {
		mode = params[0]
	}
	switch mode {
	case 0:
		for index := clampInt(capture.column, 0, capture.width); index < capture.width; index++ {
			capture.cells[capture.row][index] = ' '
		}
	case 1:
		for index := 0; index <= clampInt(capture.column, 0, capture.width-1); index++ {
			capture.cells[capture.row][index] = ' '
		}
	case 2:
		capture.cells[capture.row] = bytes.Repeat([]byte{' '}, capture.width)
	}
}

func (capture *terminalCapture) appendTranscript(value byte) {
	if value == '\n' && len(capture.transcript) > 0 && capture.transcript[len(capture.transcript)-1] == '\n' {
		return
	}
	capture.transcript = append(capture.transcript, value)
}

func (capture *terminalCapture) clear() {
	for row := range capture.cells {
		capture.cells[row] = bytes.Repeat([]byte{' '}, capture.width)
	}
	capture.row, capture.column = 0, 0
}

func (capture *terminalCapture) resize(width, height int) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.resizeLocked(width, height)
	capture.notifyLocked()
}

func (capture *terminalCapture) resizeLocked(width, height int) {
	width = max(1, width)
	height = max(1, height)
	old := capture.cells
	capture.width, capture.height = width, height
	capture.cells = make([][]byte, height)
	for row := range capture.cells {
		capture.cells[row] = bytes.Repeat([]byte{' '}, width)
		if row < len(old) {
			copy(capture.cells[row], old[row])
		}
	}
	capture.row = clampInt(capture.row, 0, height-1)
	capture.column = clampInt(capture.column, 0, width-1)
}

func (capture *terminalCapture) waitContains(marker string, timeout time.Duration, process *processResult) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		capture.mu.Lock()
		visible := capture.visibleLocked()
		transcript := string(capture.transcript)
		changed := capture.changed
		closed := capture.closed
		terminalErr := capture.terminalErr
		capture.mu.Unlock()
		if strings.Contains(visible, marker) || strings.Contains(transcript, marker) {
			return nil
		}
		if closed && terminalErr != nil {
			return terminalErr
		}
		select {
		case <-changed:
		case <-process.done:
			err := process.error()
			if err == nil {
				return fmt.Errorf("viewer exited before marker")
			}
			return fmt.Errorf("viewer exited before marker: %w", err)
		case <-deadline.C:
			return fmt.Errorf("deadline exceeded")
		}
	}
}

func (capture *terminalCapture) visible() string {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.visibleLocked()
}

func (capture *terminalCapture) visibleLocked() string {
	lines := make([]string, len(capture.cells))
	for row := range capture.cells {
		lines[row] = strings.TrimRight(string(capture.cells[row]), " ")
	}
	return strings.Join(lines, "\n")
}

func (capture *terminalCapture) tail(limit int) string {
	if capture == nil {
		return ""
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	start := max(0, len(capture.transcript)-limit)
	return string(capture.transcript[start:])
}

func (capture *terminalCapture) notifyLocked() {
	close(capture.changed)
	capture.changed = make(chan struct{})
}

func clampInt(value, minimum, maximum int) int {
	if maximum < minimum {
		return minimum
	}
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}
