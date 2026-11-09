package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const maxOutput = 16 << 20
const maxSource = 64 << 20

// Engine is a deployment registration, not an installer or a PATH search.
// NodeExecutable is the deployment's compatible Node runtime, never agent env.
type Engine struct {
	Executable     string `json:"executable"`
	NodeExecutable string `json:"nodeExecutable"`
	Version        string `json:"version"`
	Digest         string `json:"digest"`
}

func (e Engine) valid() bool {
	return e.Version == EngineVersion && filepath.IsAbs(e.Executable) && filepath.IsAbs(e.NodeExecutable) && len(e.Digest) == 64
}

func engineFiles(ctx context.Context, executable, node string) (engine Engine, root string, resultErr error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			engine, root, resultErr = Engine{}, "", err
		}
	}()
	if err := ctx.Err(); err != nil {
		return Engine{}, "", err
	}
	bad := errors.New("engine registration unavailable")
	if !filepath.IsAbs(executable) || !filepath.IsAbs(node) {
		return Engine{}, "", bad
	}
	entry, err := filepath.EvalSymlinks(executable)
	if err != nil || filepath.Base(entry) != "bin.js" || filepath.Base(filepath.Dir(entry)) != "dist" {
		return Engine{}, "", bad
	}
	pkg := filepath.Dir(filepath.Dir(entry))
	modules := filepath.Dir(pkg)
	if filepath.Base(pkg) != EngineName || filepath.Base(modules) != "node_modules" {
		return Engine{}, "", bad
	}
	metadataFile, err := os.OpenFile(filepath.Join(pkg, "package.json"), sourceOpenFlags, 0)
	if err != nil {
		return Engine{}, "", err
	}
	defer metadataFile.Close()
	info, err := metadataFile.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return Engine{}, "", bad
	}
	var metadataBytes bytes.Buffer
	_, err = copyWithContext(ctx, &metadataBytes, metadataFile)
	b := metadataBytes.Bytes()
	var metadata struct{ Name, Version string }
	if err != nil || json.Unmarshal(b, &metadata) != nil || metadata.Name != EngineName || metadata.Version != EngineVersion {
		return Engine{}, "", bad
	}
	h := sha256.New()
	err = filepath.WalkDir(modules, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, _ := filepath.Rel(modules, path)
		fmt.Fprintf(h, "%s\x00%d\x00", rel, d.Type())
		if d.Type()&os.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(path)
			if err != nil || !within(modules, target) {
				return bad
			}
			fmt.Fprintln(h, target)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return bad
		}
		f, err := os.OpenFile(path, sourceOpenFlags, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return bad
		}
		_, err = copyWithContext(ctx, h, f)
		return err
	})
	if err != nil {
		return Engine{}, "", bad
	}
	if err := ctx.Err(); err != nil {
		return Engine{}, "", err
	}
	f, err := os.OpenFile(node, sourceOpenFlags, 0)
	if err != nil {
		return Engine{}, "", bad
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return Engine{}, "", bad
	}
	if _, err := copyWithContext(ctx, h, f); err != nil {
		return Engine{}, "", bad
	}
	return Engine{entry, node, EngineVersion, hex.EncodeToString(h.Sum(nil))}, modules, nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// AuthorizedSource is supplied by AO's trusted binding/source records. There
// is no directory-discovery API: each source must name one native regular file.
type AuthorizedSource struct {
	Root     string
	Path     string // Relative to Root; OpenRoot prevents symlink escape.
	Source   string // claude or codex
	NativeID string
}

// Runner uses an explicit private temporary root beneath the AO data namespace.
// Non-Linux hosts fail closed until an equivalent OS sandbox is implemented.
// Each call has its own HOME/cache/view and is bounded even without a caller deadline.
type Runner struct{ TempRoot string }

func (r Runner) Register(ctx context.Context, executable, node, version string) (Engine, error) {
	if version != EngineVersion {
		return Engine{}, errors.New("engine version unavailable")
	}
	e, modules, err := engineFiles(ctx, executable, node)
	if err != nil {
		return Engine{}, err
	}
	dir, err := r.jobDir()
	if err != nil {
		return Engine{}, err
	}
	defer os.RemoveAll(dir)
	v, err := r.execute(ctx, dir, modules, e.NodeExecutable, "--version")
	if err != nil || strings.TrimSpace(string(v)) != EngineVersion {
		return Engine{}, errors.New("engine version unavailable")
	}
	v, err = r.sandbox(ctx, dir, modules, e.NodeExecutable, []string{"/runtime/node", "--version"})
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(v)), "v24.") {
		return Engine{}, errors.New("Node 24 runtime unavailable")
	}
	return e, nil
}

func (r Runner) jobDir() (string, error) {
	if !filepath.IsAbs(r.TempRoot) {
		return "", errors.New("report scratch unavailable")
	}
	dir, err := os.MkdirTemp(r.TempRoot, "r-")
	if err != nil {
		return "", errors.New("report scratch unavailable")
	}
	for _, child := range []string{"home", "tmp", "view"} {
		if err := os.Mkdir(filepath.Join(dir, child), 0700); err != nil {
			os.RemoveAll(dir)
			return "", errors.New("report scratch unavailable")
		}
	}
	return dir, nil
}

func copySource(ctx context.Context, dir string, index int, source AuthorizedSource) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(source.Root) || filepath.IsAbs(source.Path) || source.NativeID == "" || (source.Source != "claude" && source.Source != "codex") {
		return errors.New("untrusted_source")
	}
	r, err := os.OpenRoot(source.Root)
	if err != nil {
		return errors.New("untrusted_source")
	}
	defer r.Close()
	f, err := r.OpenFile(source.Path, sourceOpenFlags, 0)
	if err != nil {
		return errors.New("untrusted_source")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSource {
		return errors.New("untrusted_source")
	}
	// Names are opaque. Only the parser's required directory shape is retained.
	subdir := "projects"
	if source.Source == "codex" {
		subdir = "sessions"
	}
	target := filepath.Join(dir, "view", source.Source, subdir)
	if err := os.MkdirAll(target, 0700); err != nil {
		return errors.New("source_view_unavailable")
	}
	out, err := os.OpenFile(filepath.Join(target, fmt.Sprintf("source-%d.jsonl", index)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("source_view_unavailable")
	}
	n, copyErr := copyWithContext(ctx, out, io.LimitReader(f, maxSource+1))
	closeErr := out.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	if copyErr != nil || closeErr != nil || n > maxSource || n != info.Size() {
		return errors.New("source_view_unavailable")
	}
	return ctx.Err()
}

func (r Runner) execute(ctx context.Context, dir, modules, node string, args ...string) ([]byte, error) {
	return r.sandbox(ctx, dir, modules, node, append([]string{"/runtime/node", "/engine/node_modules/" + EngineName + "/dist/bin.js"}, args...))
}

func (r Runner) sandbox(ctx context.Context, dir, modules, node string, command []string) ([]byte, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("sandbox_unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"--unshare-net", "--unshare-pid", "--unshare-ipc", "--die-with-parent", "--new-session", "--cap-drop", "ALL", "--clearenv",
		"--ro-bind", "/usr", "/usr", "--symlink", "usr/bin", "/bin", "--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64",
		"--proc", "/proc", "--dev", "/dev", "--ro-bind", node, "/runtime/node", "--ro-bind", modules, "/engine/node_modules",
		"--ro-bind", filepath.Join(dir, "view"), "/view", "--bind", filepath.Join(dir, "home"), "/home/report", "--bind", filepath.Join(dir, "tmp"), "/tmp",
		"--setenv", "HOME", "/home/report", "--setenv", "TMPDIR", "/tmp", "--setenv", "PATH", "/runtime:/usr/bin:/bin", "--setenv", "TZ", "UTC",
		"--setenv", "LANG", "C.UTF-8", "--setenv", "LLM_USAGE_SKIP_UPDATE_CHECK", "1", "--chdir", "/view", "--"}
	cmd := exec.CommandContext(ctx, "/usr/bin/bwrap", append(args, command...)...)
	cmd.Env = []string{} // No AO, harness, provider or account variables inherited.
	var output boundedOutput
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || output.overflow {
		return nil, errors.New("engine_execution_failed")
	}
	return output.Bytes(), nil
}

type boundedOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxOutput {
		b.overflow = true
		return 0, errors.New("report output limit")
	}
	return b.Buffer.Write(p)
}
