/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package cgroup2

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

//nolint:staticcheck // Staticcheck false positives for nil pointer deference after t.Fatal
func TestEventChanCleanupOnCgroupRemoval(t *testing.T) {
	checkCgroupMode(t)

	cmd := exec.Command("cat")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err, "failed to create cat process")

	err = cmd.Start()
	require.NoError(t, err, "failed to start cat process")

	proc := cmd.Process
	require.NotNil(t, proc, "process was nil")

	group := fmt.Sprintf("testing-watcher-%d.scope", proc.Pid)
	c, err := NewSystemd("", group, proc.Pid, &Resources{})
	require.NoError(t, err, "failed to init new cgroup manager")

	evCh, errCh := c.EventChan()

	// give event goroutine a chance to start
	time.Sleep(500 * time.Millisecond)

	err = stdin.Close()
	require.NoError(t, err, "failed closing stdin")

	err = cmd.Wait()
	require.NoError(t, err, "failed waiting for cmd")

	done := false
	for !done {
		select {
		case <-evCh:
		case err := <-errCh:
			require.NoError(t, err, "unexpected error on error channel")
			done = true
		case <-time.After(5 * time.Second):
			t.Fatal("Timed out")
		}
	}
	goleak.VerifyNone(t)
}

func TestSystemdFullPath(t *testing.T) {
	tests := []struct {
		inputSlice  string
		inputGroup  string
		expectedOut string
	}{
		{
			inputSlice:  "user.slice",
			inputGroup:  "myGroup.slice",
			expectedOut: "/sys/fs/cgroup/user.slice/myGroup.slice",
		},
		{
			inputSlice:  "/",
			inputGroup:  "myGroup.slice",
			expectedOut: "/sys/fs/cgroup/myGroup.slice",
		},
		{
			inputSlice:  "system.slice",
			inputGroup:  "myGroup.slice",
			expectedOut: "/sys/fs/cgroup/system.slice/myGroup.slice",
		},
		{
			inputSlice:  "user.slice",
			inputGroup:  "my-group.slice",
			expectedOut: "/sys/fs/cgroup/user.slice/my.slice/my-group.slice",
		},
		{
			inputSlice:  "user.slice",
			inputGroup:  "my-group-more-dashes.slice",
			expectedOut: "/sys/fs/cgroup/user.slice/my.slice/my-group.slice/my-group-more.slice/my-group-more-dashes.slice",
		},
		{
			inputSlice:  "user.slice",
			inputGroup:  "my-group-dashes.slice",
			expectedOut: "/sys/fs/cgroup/user.slice/my.slice/my-group.slice/my-group-dashes.slice",
		},
		{
			inputSlice:  "user.slice",
			inputGroup:  "myGroup.scope",
			expectedOut: "/sys/fs/cgroup/user.slice/myGroup.scope",
		},
		{
			inputSlice:  "user.slice",
			inputGroup:  "my-group-dashes.scope",
			expectedOut: "/sys/fs/cgroup/user.slice/my-group-dashes.scope",
		},
		{
			inputSlice:  "test-waldo.slice",
			inputGroup:  "my-group.slice",
			expectedOut: "/sys/fs/cgroup/test.slice/test-waldo.slice/my.slice/my-group.slice",
		},
		{
			inputSlice:  "test-waldo.slice",
			inputGroup:  "my.service",
			expectedOut: "/sys/fs/cgroup/test.slice/test-waldo.slice/my.service",
		},
	}

	for _, test := range tests {
		actual := getSystemdFullPath(test.inputSlice, test.inputGroup)
		assert.Equal(t, test.expectedOut, actual)
	}
}

func TestKill(t *testing.T) {
	checkCgroupMode(t)
	manager, err := NewManager(defaultCgroup2Path, "/test1", ToResources(&specs.LinuxResources{}))
	require.NoError(t, err)

	var (
		procs    []*exec.Cmd
		numProcs = 5
	)
	for i := 0; i < numProcs; i++ {
		cmd := exec.Command("sleep", "infinity")
		// Don't leak the process if we fail to join the cg,
		// send sigkill after tests over.
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Pdeathsig: syscall.SIGKILL,
		}
		err = cmd.Start()
		require.NoError(t, err)

		err = manager.AddProc(uint64(cmd.Process.Pid))
		require.NoError(t, err)

		procs = append(procs, cmd)
	}
	// Verify we have 5 pids before beginning Kill below.
	pids, err := manager.Procs(true)
	require.NoError(t, err)
	require.Len(t, pids, numProcs, "pid count unexpected")

	// Now run kill, and check that nothing is running after.
	err = manager.Kill()
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		for _, proc := range procs {
			_ = proc.Wait()
		}
		done <- struct{}{}
	}()

	select {
	case <-time.After(time.Second * 3):
		t.Fatal("timed out waiting for processes to exit")
	case <-done:
	}
}

func TestMoveTo(t *testing.T) {
	checkCgroupMode(t)
	manager, err := NewManager(defaultCgroup2Path, "/test1", ToResources(&specs.LinuxResources{}))
	require.NoError(t, err)

	proc := os.Getpid()
	err = manager.AddProc(uint64(proc))
	require.NoError(t, err)

	destination, err := NewManager(defaultCgroup2Path, "/test2", ToResources(&specs.LinuxResources{}))
	require.NoError(t, err)

	err = manager.MoveTo(destination)
	require.NoError(t, err)

	desProcs, err := destination.Procs(true)
	require.NoError(t, err)

	desMap := make(map[int]bool)
	for _, p := range desProcs {
		desMap[int(p)] = true
	}
	if !desMap[proc] {
		t.Fatalf("process %v not in destination cgroup", proc)
	}
}

func TestCgroupType(t *testing.T) {
	checkCgroupMode(t)
	manager, err := NewManager(defaultCgroup2Path, "/test1", ToResources(&specs.LinuxResources{}))
	require.NoError(t, err)

	cgType, err := manager.GetType()
	require.NoError(t, err)
	require.Equal(t, cgType, Domain)

	// Swap to threaded
	require.NoError(t, manager.SetType(Threaded))

	cgType, err = manager.GetType()
	require.NoError(t, err)
	require.Equal(t, cgType, Threaded)
}
