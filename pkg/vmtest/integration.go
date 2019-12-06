// Copyright 2018 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vmtest

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/u-root/u-root/pkg/cp"
	"github.com/u-root/u-root/pkg/golang"
	"github.com/u-root/u-root/pkg/qemu"
	"github.com/u-root/u-root/pkg/uio"
	"github.com/u-root/u-root/pkg/ulog"
	"github.com/u-root/u-root/pkg/ulog/ulogtest"
	"github.com/u-root/u-root/pkg/uroot"
	"github.com/u-root/u-root/pkg/uroot/initramfs"
)

// Options are integration test options.
type Options struct {
	// BuildOpts are u-root initramfs options.
	//
	// They are used if the test needs to generate an initramfs.
	// Fields that are not set are populated by QEMU and QEMUTest as
	// possible.
	BuildOpts uroot.Opts

	// QEMUOpts are QEMU VM options for the test.
	//
	// Fields that are not set are populated by QEMU and QEMUTest as
	// possible.
	QEMUOpts qemu.Options

	// DontSetEnv doesn't set the BuildOpts.Env and uses the user-supplied one.
	//
	// TODO: make uroot.Opts.Env a pointer?
	DontSetEnv bool

	// Name is the test's name.
	//
	// If name is left empty, the calling function's function name will be
	// used as determined by runtime.Caller.
	Name string

	// Uinit is the uinit that should be added to a generated initramfs.
	//
	// If none is specified, the generic uinit will be used, which searches for
	// and runs the script generated from TestCmds.
	Uinit string

	// TestCmds are commands to execute after init.
	//
	// QEMUTest generates an Elvish script with these commands. The script is
	// shared with the VM, and is run from the generic uinit.
	TestCmds []string

	// TmpDir is the temporary directory exposed to the QEMU VM.
	TmpDir string

	// Logger logs build statements.
	Logger ulog.Logger

	// Extra environment variables to set when building (used by u-bmc)
	ExtraBuildEnv []string

	// Use virtual vfat rather than 9pfs
	UseVVFAT bool
}

func last(s string) string {
	l := strings.Split(s, ".")
	return l[len(l)-1]
}

func callerName(depth int) string {
	// Use the test name as the serial log's file name.
	pc, _, _, ok := runtime.Caller(depth)
	if !ok {
		panic("runtime caller failed")
	}
	f := runtime.FuncForPC(pc)
	return last(f.Name())
}

// TestLineWriter is an io.Writer that logs full lines of serial to tb.
func TestLineWriter(tb testing.TB, prefix string) io.WriteCloser {
	return uio.FullLineWriter(&testLineWriter{tb: tb, prefix: prefix})
}

type jsonStripper struct {
	uio.LineWriter
}

func (j jsonStripper) OneLine(p []byte) {
	// Poor man's JSON detector.
	if len(p) == 0 || p[0] == '{' {
		return
	}
	j.LineWriter.OneLine(p)
}

func JSONLessTestLineWriter(tb testing.TB, prefix string) io.WriteCloser {
	return uio.FullLineWriter(jsonStripper{&testLineWriter{tb: tb, prefix: prefix}})
}

// testLineWriter is an io.Writer that logs full lines of serial to tb.
type testLineWriter struct {
	tb     testing.TB
	prefix string
}

func (tsw *testLineWriter) OneLine(p []byte) {
	tsw.tb.Logf("%s: %s", tsw.prefix, strings.ReplaceAll(string(p), "\033", "~"))
}

// TestArch returns the architecture under test. Pass this as GOARCH when
// building Go programs to be run in the QEMU environment.
func TestArch() string {
	if env := os.Getenv("UROOT_TESTARCH"); env != "" {
		return env
	}
	return "amd64"
}

// SkipWithoutQEMU skips the test when the QEMU environment variables are not
// set. This is already called by QEMUTest(), so use if some expensive
// operations are performed before calling QEMUTest().
func SkipWithoutQEMU(t *testing.T) {
	if _, ok := os.LookupEnv("UROOT_QEMU"); !ok {
		t.Skip("QEMU test is skipped unless UROOT_QEMU is set")
	}
	if _, ok := os.LookupEnv("UROOT_KERNEL"); !ok {
		t.Skip("QEMU test is skipped unless UROOT_KERNEL is set")
	}
}

func QEMUTest(t *testing.T, o *Options) (*qemu.VM, func()) {
	SkipWithoutQEMU(t)

	if len(o.Name) == 0 {
		o.Name = callerName(2)
	}
	if o.Logger == nil {
		o.Logger = &ulogtest.Logger{TB: t}
	}
	if o.QEMUOpts.SerialOutput == nil {
		o.QEMUOpts.SerialOutput = TestLineWriter(t, "serial")
	}

	// Create or reuse a temporary directory. This is exposed to the VM.
	if o.TmpDir == "" {
		tmpDir, err := ioutil.TempDir("", "uroot-integration")
		if err != nil {
			t.Fatalf("Failed to create temp dir: %v", err)
		}
		o.TmpDir = tmpDir
	}

	qOpts, err := QEMU(o)
	if err != nil {
		t.Fatalf("Failed to create QEMU VM %s: %v", o.Name, err)
	}

	vm, err := qOpts.Start()
	if err != nil {
		t.Fatalf("Failed to start QEMU VM %s: %v", o.Name, err)
	}
	t.Logf("QEMU command line for %s:\n%s", o.Name, vm.CmdlineQuoted())

	return vm, func() {
		vm.Close()
		if t.Failed() {
			t.Log("Keeping temp dir: ", o.TmpDir)
		} else if len(o.TmpDir) == 0 {
			if err := os.RemoveAll(o.TmpDir); err != nil {
				t.Logf("failed to remove temporary directory %s: %v", o.TmpDir, err)
			}
		}
	}
}

// QEMU builds the u-root environment and prepares QEMU options given the test
// options and environment variables.
//
// QEMU will augment o.BuildOpts and o.QEMUOpts with configuration that the
// caller either requested (through the Options.Uinit field, for example) or
// that the caller did not set.
//
// QEMU returns the QEMU launch options or an error.
func QEMU(o *Options) (*qemu.Options, error) {
	if len(o.Name) == 0 {
		o.Name = callerName(2)
	}

	// Generate Elvish shell script of test commands in o.TmpDir.
	if len(o.TestCmds) > 0 {
		testFile := filepath.Join(o.TmpDir, "test.elv")

		if err := ioutil.WriteFile(
			testFile, []byte(strings.Join(o.TestCmds, "\n")), 0777); err != nil {
			return nil, err
		}
	}

	// Create initramfs if caller did not provide one.
	initramfs := os.Getenv("UROOT_INITRAMFS")
	if len(initramfs) == 0 {
		initramfs = filepath.Join(o.TmpDir, "initramfs.cpio")
		if err := CreateTestInitramfs(o.BuildOpts, o.Uinit, initramfs); err != nil {
			return nil, err
		}
	}
	o.QEMUOpts.Initramfs = initramfs

	if len(o.QEMUOpts.Kernel) == 0 {
		// Copy kernel to o.TmpDir for tests involving kexec.
		kernel := filepath.Join(o.TmpDir, "kernel")
		if err := cp.Copy(os.Getenv("UROOT_KERNEL"), kernel); err != nil {
			return nil, err
		}
		o.QEMUOpts.Kernel = kernel
	}

	switch TestArch() {
	case "amd64":
		o.QEMUOpts.KernelArgs += " console=ttyS0 earlyprintk=ttyS0"
	case "arm":
		o.QEMUOpts.KernelArgs += " console=ttyAMA0"
	}

	var dir qemu.Device
	if o.UseVVFAT {
		dir = qemu.ReadOnlyDirectory{Dir: o.TmpDir}
	} else {
		dir = qemu.P9Directory{Dir: o.TmpDir, Arch: TestArch()}
	}
	o.QEMUOpts.Devices = append(o.QEMUOpts.Devices, qemu.VirtioRandom{}, dir)

	return &o.QEMUOpts, nil
}

// CreateTestInitramfs creates an initramfs with the given build options and
// uinit, and writes it to the given output file.
func CreateTestInitramfs(o uroot.Opts, uinit, outputFile string) error {
	// TODO need to bring back the DontSetEnv check?
	env := golang.Default()
	env.CgoEnabled = false
	env.GOARCH = TestArch()
	o.Env = env

	logger := log.New(os.Stderr, "", 0)

	// If build opts don't specify any commands, include all core commands. Else,
	// always add init and elvish.
	var cmds []string
	if len(o.Commands) == 0 {
		cmds = []string{"github.com/u-root/u-root/cmds/core/*"}
	} else {
		cmds = []string{
			"github.com/u-root/u-root/cmds/core/init",
			"github.com/u-root/u-root/cmds/core/elvish",
		}
	}

	// If a custom uinit was not provided, use the generic test uinit. This will
	// try to find and run the test commands shell script.
	if len(uinit) == 0 {
		uinit = "github.com/u-root/u-root/integration/testcmd/generic/uinit"
	}
	cmds = append(cmds, uinit)

	// Add our commands to the build opts.
	o.AddBusyBoxCommands(cmds...)

	// Fill in the default build options if not specified.
	if o.BaseArchive == nil {
		o.BaseArchive = uroot.DefaultRamfs.Reader()
	}
	if len(o.InitCmd) == 0 {
		o.InitCmd = "init"
	}
	if len(o.DefaultShell) == 0 {
		o.DefaultShell = "elvish"
	}
	if len(o.TempDir) == 0 {
		tempDir, err := ioutil.TempDir("", "initramfs-tempdir")
		if err != nil {
			return fmt.Errorf("Failed to create temp dir: %v", err)
		}
		defer os.RemoveAll(tempDir)
		o.TempDir = tempDir
	}

	if o.OutputFile == nil {
		if len(outputFile) == 0 {
			return fmt.Errorf("No output file provided.")
		}
		w, err := initramfs.CPIO.OpenWriter(logger, outputFile, "", "")
		if err != nil {
			return fmt.Errorf("Failed to create initramfs writer: %v", err)
		}
		o.OutputFile = w
	}

	return uroot.CreateInitramfs(logger, o)
}
