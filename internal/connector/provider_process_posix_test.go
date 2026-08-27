//go:build darwin || linux

package connector_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/connector"
)

const providerTreeHelperEnvironment = "TASKAI_TEST_PROVIDER_TREE_HELPER"

func TestProviderProcessCloseTerminatesForkedProcessGroup(t *testing.T) {
	if os.Getenv(providerTreeHelperEnvironment) != "" {
		return
	}
	t.Setenv(providerTreeHelperEnvironment, "leader")
	process, err := (connector.Launcher{}).Start(context.Background(), connector.CommandSpec{
		Executable: os.Args[0], Args: []string{"-test.run=^TestProviderProcessTreeHelper$"},
	})
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(process.Stdout)
	line, err := reader.ReadString('\n')
	if err != nil {
		_ = process.Close()
		t.Fatal(err)
	}
	fields := strings.Fields(line)
	if len(fields) != 2 {
		_ = process.Close()
		t.Fatalf("helper pid line = %q", line)
	}
	leaderPID, leaderErr := strconv.Atoi(fields[0])
	childPID, childErr := strconv.Atoi(fields[1])
	if leaderErr != nil || childErr != nil || leaderPID != process.PID() || childPID <= 0 {
		_ = process.Close()
		t.Fatalf("helper pids = %q", line)
	}
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{leaderPID, childPID} {
		deadline := time.Now().Add(5 * time.Second)
		for processExists(pid) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if processExists(pid) {
			t.Fatalf("provider process %d survived process-group Close", pid)
		}
	}
}

func TestProviderProcessTreeHelper(t *testing.T) {
	switch os.Getenv(providerTreeHelperEnvironment) {
	case "":
		return
	case "grandchild":
		select {}
	case "leader":
		command := exec.Command(os.Args[0], "-test.run=^TestProviderProcessTreeHelper$")
		command.Env = replaceEnvironment(os.Environ(), providerTreeHelperEnvironment, "grandchild")
		if err := command.Start(); err != nil {
			os.Exit(71)
		}
		fmt.Printf("%d %d\n", os.Getpid(), command.Process.Pid)
		select {}
	default:
		os.Exit(72)
	}
}

func replaceEnvironment(values []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(values)+1)
	for _, item := range values {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return append(result, prefix+value)
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
