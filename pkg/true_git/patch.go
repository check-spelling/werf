package true_git

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/werf/werf/pkg/path_matcher"
)

type PatchOptions struct {
	FromCommit, ToCommit string
	PathMatcher          path_matcher.PathMatcher

	WithEntireFileContext bool
	WithBinary            bool
}

type PatchDescriptor struct {
	Paths       []string
	BinaryPaths []string
}

func PatchWithSubmodules(ctx context.Context, out io.Writer, gitDir, workTreeCacheDir string, opts PatchOptions) (*PatchDescriptor, error) {
	var res *PatchDescriptor

	err := withWorkTreeCacheLock(ctx, workTreeCacheDir, func() error {
		writePatchRes, err := writePatch(ctx, out, gitDir, workTreeCacheDir, true, opts)
		res = writePatchRes
		return err
	})

	return res, err
}

func Patch(ctx context.Context, out io.Writer, gitDir string, opts PatchOptions) (*PatchDescriptor, error) {
	return writePatch(ctx, out, gitDir, "", false, opts)
}

func debugPatch() bool {
	return os.Getenv("WERF_TRUE_GIT_DEBUG_PATCH") == "1"
}

func writePatch(ctx context.Context, out io.Writer, gitDir, workTreeCacheDir string, withSubmodules bool, opts PatchOptions) (*PatchDescriptor, error) {
	var err error

	gitDir, err = filepath.Abs(gitDir)
	if err != nil {
		return nil, fmt.Errorf("bad git dir %s: %s", gitDir, err)
	}

	workTreeCacheDir, err = filepath.Abs(workTreeCacheDir)
	if err != nil {
		return nil, fmt.Errorf("bad work tree cache dir %s: %s", workTreeCacheDir, err)
	}

	if withSubmodules {
		err := checkSubmoduleConstraint()
		if err != nil {
			return nil, err
		}
	}
	if withSubmodules && workTreeCacheDir == "" {
		return nil, fmt.Errorf("provide work tree cache directory to enable submodules!")
	}

	commonGitOpts := []string{
		"-c", "diff.renames=false",
		"-c", "core.quotePath=false",
	}
	if opts.WithEntireFileContext {
		commonGitOpts = append(commonGitOpts, "-c", "diff.context=999999999")
	}

	diffOpts := []string{"--full-index"}
	if withSubmodules {
		diffOpts = append(diffOpts, "--submodule=diff")
	} else {
		diffOpts = append(diffOpts, "--submodule=log")
	}
	if opts.WithBinary {
		diffOpts = append(diffOpts, "--binary")
	}

	var cmd *exec.Cmd

	if withSubmodules {
		workTreeDir, err := prepareWorkTree(ctx, gitDir, workTreeCacheDir, opts.ToCommit, withSubmodules)
		if err != nil {
			return nil, fmt.Errorf("cannot prepare work tree in cache %s for commit %s: %s", workTreeCacheDir, opts.ToCommit, err)
		}

		gitArgs := append(commonGitOpts, "-C", workTreeDir)
		gitArgs = append(gitArgs, "diff")
		gitArgs = append(gitArgs, diffOpts...)
		gitArgs = append(gitArgs, opts.FromCommit, opts.ToCommit)

		if debugPatch() {
			fmt.Printf("# git %s\n", strings.Join(gitArgs, " "))
		}

		cmd = exec.Command("git", gitArgs...)

		cmd.Dir = workTreeDir // required for `git diff` with submodules
	} else {
		gitArgs := append(commonGitOpts, "-C", gitDir)
		gitArgs = append(gitArgs, "diff")
		gitArgs = append(gitArgs, diffOpts...)
		gitArgs = append(gitArgs, opts.FromCommit, opts.ToCommit)

		if debugPatch() {
			fmt.Printf("# git %s\n", strings.Join(gitArgs, " "))
		}

		cmd = exec.Command("git", gitArgs...)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("error creating git diff stdout pipe: %s", err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("error creating git diff stderr pipe: %s", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("error starting git diff: %s", err)
	}

	outputErrChan := make(chan error)
	stdoutChan := make(chan []byte)
	stderrChan := make(chan []byte)
	doneChan := make(chan bool)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		err := consumePipeOutput(stdoutPipe, func(data []byte) error {
			dataCopy := make([]byte, len(data))
			copy(dataCopy, data)

			stdoutChan <- dataCopy

			return nil
		})
		if err != nil {
			outputErrChan <- err
		}

		wg.Done()
	}()

	wg.Add(1)
	go func() {
		err := consumePipeOutput(stderrPipe, func(data []byte) error {
			dataCopy := make([]byte, len(data))
			copy(dataCopy, data)

			stderrChan <- dataCopy

			return nil
		})
		if err != nil {
			outputErrChan <- err
		}

		wg.Done()
	}()

	go func() {
		wg.Wait()
		doneChan <- true
	}()

	if debugPatch() {
		out = io.MultiWriter(out, os.Stdout)
	}

	p := makeDiffParser(out, opts.PathMatcher)

WaitForData:
	for {
		select {
		case err := <-outputErrChan:
			return nil, fmt.Errorf("error getting git diff output: %s\nunrecognized output:\n%s", err, p.UnrecognizedCapture.String())
		case stdoutData := <-stdoutChan:
			if err := p.HandleStdout(stdoutData); err != nil {
				return nil, err
			}
		case stderrData := <-stderrChan:
			if err := p.HandleStderr(stderrData); err != nil {
				return nil, err
			}
		case <-doneChan:
			break WaitForData
		}
	}

	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("git diff error: %s\nunrecognized output:\n%s", err, p.UnrecognizedCapture.String())
	}

	desc := &PatchDescriptor{
		Paths:       p.Paths,
		BinaryPaths: p.BinaryPaths,
	}

	if debugPatch() {
		fmt.Printf("Patch paths count is %d, binary paths count is %d\n", len(desc.Paths), len(desc.BinaryPaths))
		for _, path := range desc.Paths {
			fmt.Printf("Patch path %s\n", path)
		}
		for _, path := range desc.BinaryPaths {
			fmt.Printf("Binary patch path %s\n", path)
		}
	}

	return desc, nil
}

func consumePipeOutput(pipe io.ReadCloser, handleChunk func(data []byte) error) error {
	chunkBuf := make([]byte, 1024*64)

	for {
		n, err := pipe.Read(chunkBuf)
		if n > 0 {
			handleErr := handleChunk(chunkBuf[:n])
			if handleErr != nil {
				return handleErr
			}
		}

		if err == io.EOF {
			err := pipe.Close()
			if err != nil {
				return fmt.Errorf("error closing pipe: %s", err)
			}

			return nil
		}

		if err != nil {
			return fmt.Errorf("error reading pipe: %s", err)
		}
	}
}
