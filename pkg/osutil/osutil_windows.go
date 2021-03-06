// Copyright 2017 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

// +build !appengine

package osutil

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func HandleInterrupts(shutdown chan struct{}) {
}

func UmountAll(dir string) {
}

func prolongPipe(r, w *os.File) {
}

func CreateMemMappedFile(size int) (f *os.File, mem []byte, err error) {
	return nil, nil, fmt.Errorf("CreateMemMappedFile is not implemented")
}

func CloseMemMappedFile(f *os.File, mem []byte) error {
	return fmt.Errorf("CloseMemMappedFile is not implemented")
}

func ProcessExitStatus(ps *os.ProcessState) int {
	return ps.Sys().(syscall.WaitStatus).ExitStatus()
}

func ProcessSignal(p *os.Process, sig int) bool {
	return false
}

func Sandbox(cmd *exec.Cmd, user, net bool) error {
	return nil
}

func SandboxChown(file string) error {
	return nil
}

func setPdeathsig(cmd *exec.Cmd) {
}
