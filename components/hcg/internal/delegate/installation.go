package delegate

import (
	"bufio"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const fixedPALSourceDigest = "bac632c7fbe275c2f9389be3a56e69ada6a1d35d29cbc80ce575a46389608f4d"
const fixedDependencyLockDigest = "799a72fd49b931a9f50152a6342f574efa1eb8eb89ba500e3f3e7f5859357666"

//go:embed pal-source-files.txt
var fixedPALFiles string

//go:embed pal-requirements.lock
var fixedDependencyLock string

func validDigest(s string) bool { b, err := hex.DecodeString(s); return err == nil && len(b) == 32 }
func validateDeployment(d Deployment) error {
	p := d.Profile
	i := d.Installation
	if !p.valid() || p.DeploymentDigest != "" || d.CredentialRef.Provider != p.BillingProvider || !profileToken.MatchString(d.CredentialRef.Name) || !safeText(d.CredentialRef.Name) || !profileToken.MatchString(d.CredentialRef.Version) || !profileToken.MatchString(d.Workspace) || !safeText(d.Workspace) || len(d.Inputs) == 0 || len(d.Inputs) > 64 {
		return ErrUnavailable
	}
	if _, err := worstCharge(p); err != nil {
		return err
	}
	if i.Commit != PALCommit || i.SourceDigest != fixedPALSourceDigest || i.LockDigest != fixedDependencyLockDigest || i.PythonVersion != "3.12.3" || i.AdapterVersion != "hcg-pal/1" || i.TemplateVersion != "hcg.delegate-read/1" || !validDigest(i.RuntimeDigest) || !validDigest(i.PythonDigest) || !validDigest(i.ConfigDigest) || !filepath.IsAbs(i.Source) || !filepath.IsAbs(i.Venv) {
		return ErrUnavailable
	}
	projects := map[string]bool{}
	for _, input := range d.Inputs {
		if input.ProjectID == "" || len(input.ProjectID) > 256 || projects[input.ProjectID] || !profileToken.MatchString(input.Name) || !profileToken.MatchString(input.Revision) || len(input.Files) == 0 || len(input.Files) > 256 {
			return ErrUnavailable
		}
		projects[input.ProjectID] = true
		files := map[string]bool{}
		for _, file := range input.Files {
			if !filepath.IsLocal(file.Path) || filepath.Clean(file.Path) != file.Path || !safeSourcePath(file.Path) || !validDigest(file.SHA256) || files[file.Path] {
				return ErrUnavailable
			}
			files[file.Path] = true
		}
	}
	return nil
}
func verifyInstallation(ctx context.Context, i Installation) error {
	source, err := snapshotTree(ctx, i.Source, "", true)
	if err != nil || source != i.SourceDigest {
		return ErrUnavailable
	}
	site := filepath.Join(i.Venv, "lib", "python3.12", "site-packages")
	runtime, err := snapshotTree(ctx, site, "", false)
	if err != nil || runtime != i.RuntimeDigest {
		return ErrUnavailable
	}
	if err := verifyDependencyVersions(ctx, site); err != nil {
		return err
	}
	cfg, err := os.ReadFile(filepath.Join(i.Venv, "pyvenv.cfg"))
	if err != nil || len(cfg) > 8192 || textHash(string(cfg)) != i.ConfigDigest {
		return ErrUnavailable
	}
	python, err := os.Open(filepath.Join(i.Venv, "bin", "python"))
	if err != nil {
		return ErrUnavailable
	}
	defer python.Close()
	hash := sha256.New()
	if _, err := copySnapshot(ctx, hash, python, 64<<20); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != i.PythonDigest {
		return ErrUnavailable
	}
	return nil
}

// WalkDir's component/preorder sorting is the same order used by the approved
// Python Path inventory, not a flat lexicographic sort of slash-joined names.
func snapshotTree(ctx context.Context, root, destination string, pal bool) (string, error) {
	expected := map[string]bool{}
	directories := map[string]bool{}
	if pal {
		for _, name := range strings.Fields(fixedPALFiles) {
			expected[name] = true
			for dir := filepath.ToSlash(filepath.Dir(name)); dir != "."; dir = filepath.ToSlash(filepath.Dir(dir)) {
				directories[dir] = true
			}
		}
	}
	aggregate := sha256.New()
	total := int64(0)
	count := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return ErrUnavailable
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return ErrUnavailable
		}
		name := filepath.ToSlash(rel)
		if name == "." {
			return nil
		}
		if pal && name == ".git" {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			if pal && !directories[name] {
				return ErrUnavailable
			}
			if destination != "" {
				return os.MkdirAll(filepath.Join(destination, rel), 0700)
			}
			return nil
		}
		if !entry.Type().IsRegular() || (pal && !expected[name]) {
			return ErrUnavailable
		}
		count++
		if count > 6000 {
			return ErrUnavailable
		}
		f, err := os.OpenFile(path, sourceOpenFlags, 0)
		if err != nil {
			return ErrUnavailable
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			return ErrUnavailable
		}
		hash := sha256.New()
		var output *os.File
		writer := io.Writer(hash)
		if destination != "" {
			output, err = os.OpenFile(filepath.Join(destination, rel), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				f.Close()
				return ErrUnavailable
			}
			writer = io.MultiWriter(hash, output)
		}
		n, copyErr := copySnapshot(ctx, writer, f, 64<<20)
		closeErr := f.Close()
		if output != nil {
			if err := output.Close(); closeErr == nil {
				closeErr = err
			}
		}
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return ErrUnavailable
		}
		total += n
		if total > 128<<20 {
			return ErrUnavailable
		}
		_, _ = aggregate.Write([]byte(name))
		_, _ = aggregate.Write([]byte{0})
		_, _ = aggregate.Write(hash.Sum(nil))
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", ErrUnavailable
	}
	if pal && count != len(expected) {
		return "", ErrUnavailable
	}
	return hex.EncodeToString(aggregate.Sum(nil)), nil
}
func copySnapshot(ctx context.Context, dst io.Writer, src io.Reader, max int64) (int64, error) {
	buf := make([]byte, 32<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := src.Read(buf)
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		total += int64(n)
		if total > max {
			return total, ErrUnavailable
		}
		if n > 0 {
			written, writeErr := dst.Write(buf[:n])
			if writeErr != nil || written != n {
				return total, ErrUnavailable
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, ErrUnavailable
		}
	}
}
func verifyDependencyVersions(ctx context.Context, site string) error {
	expected := map[string]string{}
	for _, line := range strings.Fields(fixedDependencyLock) {
		name, version, ok := strings.Cut(line, "==")
		if !ok {
			return ErrUnavailable
		}
		expected[normalizeDistribution(name)] = version
	}
	entries, err := os.ReadDir(site)
	if err != nil {
		return ErrUnavailable
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasSuffix(entry.Name(), ".dist-info") {
			continue
		}
		f, err := os.Open(filepath.Join(site, entry.Name(), "METADATA"))
		if err != nil {
			return ErrUnavailable
		}
		scanner := bufio.NewScanner(io.LimitReader(f, 65536))
		name, version := "", ""
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "Name: ") {
				name = normalizeDistribution(strings.TrimPrefix(line, "Name: "))
			}
			if strings.HasPrefix(line, "Version: ") {
				version = strings.TrimPrefix(line, "Version: ")
			}
			if name != "" && version != "" {
				break
			}
		}
		f.Close()
		if name == "pip" && expected[name] == "" {
			continue
		}
		if expected[name] != version || version == "" || seen[name] {
			return ErrUnavailable
		}
		seen[name] = true
	}
	if len(seen) != len(expected) {
		return ErrUnavailable
	}
	return nil
}
func normalizeDistribution(s string) string { return strings.ReplaceAll(strings.ToLower(s), "_", "-") }

func stageInstallation(ctx context.Context, jobDir string, i Installation) (PAL, error) {
	source := filepath.Join(jobDir, "pal")
	venv := filepath.Join(jobDir, "v")
	site := filepath.Join(venv, "lib", "python3.12", "site-packages")
	for _, dir := range []string{source, site, filepath.Join(venv, "bin")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return PAL{}, ErrUnavailable
		}
	}
	sourceHash, err := snapshotTree(ctx, i.Source, source, true)
	if err != nil || sourceHash != i.SourceDigest {
		return PAL{}, ErrUnavailable
	}
	runtimeHash, err := snapshotTree(ctx, filepath.Join(i.Venv, "lib", "python3.12", "site-packages"), site, false)
	if err != nil || runtimeHash != i.RuntimeDigest {
		return PAL{}, ErrUnavailable
	}
	for _, name := range []string{"pyvenv.cfg", "bin/python"} {
		original, err := os.Open(filepath.Join(i.Venv, name))
		if err != nil {
			return PAL{}, ErrUnavailable
		}
		mode := os.FileMode(0600)
		if name == "bin/python" {
			mode = 0700
		}
		copy, err := os.OpenFile(filepath.Join(venv, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			original.Close()
			return PAL{}, ErrUnavailable
		}
		hash := sha256.New()
		_, err = copySnapshot(ctx, io.MultiWriter(copy, hash), original, 64<<20)
		original.Close()
		closeErr := copy.Close()
		if err != nil || closeErr != nil {
			return PAL{}, ErrUnavailable
		}
		if (name == "bin/python" && hex.EncodeToString(hash.Sum(nil)) != i.PythonDigest) || (name == "pyvenv.cfg" && hex.EncodeToString(hash.Sum(nil)) != i.ConfigDigest) {
			return PAL{}, ErrUnavailable
		}
	}
	return PAL{Python: filepath.Join(venv, "bin", "python"), Source: source, TempRoot: jobDir}, nil
}

func validateSavedDeployments(s deploymentState) error {
	if s.Grant != nil && !s.Grant.valid(s.Grant.OwnerRoot) {
		return ErrUnavailable
	}
	for key, d := range s.History {
		original := d.Profile.DeploymentDigest
		digest := d.Profile.digest()
		d.Profile.DeploymentDigest = ""
		if validateDeployment(d) != nil {
			return ErrUnavailable
		}
		raw, _ := json.Marshal(d)
		if original != textHash(string(raw)) || digest != key {
			return ErrUnavailable
		}
	}
	for id, key := range s.Current {
		d, ok := s.History[key]
		if !ok || id != d.Profile.ID {
			return ErrUnavailable
		}
	}
	return nil
}
