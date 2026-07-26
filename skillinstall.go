package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// skillDest is where the vendored skill lands inside a product — the
// directory Claude Code discovers project skills in.
const skillDest = ".claude/skills/ultrastack"

// skillManifest marks a vendored skill as machine-owned and records which
// framework version it mirrors. Its presence is what lets install overwrite
// freely and lets --check compare against go.mod.
const skillManifest = ".ultra-skill.json"

type skillMeta struct {
	Version string `json:"version"` // the go.mod pin, or "replace:<dir>"
	Source  string `json:"source"`  // the directory the skill was copied from
}

// cmdSkillInstall vendors SKILL.md + references/ from the EXACT framework
// version the product pins, so the doctrine an agent reads can never be
// newer or older than the code it builds against (the version-skew class
// from the field reports). A replace directive wins over the pin: dev
// checkouts vendor the live skill.
func cmdSkillInstall(args []string, out, errW io.Writer) int {
	check, force := false, false
	dir := "."
	for _, a := range args {
		switch a {
		case "--check":
			check = true
		case "--force":
			force = true
		default:
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(errW, "Error: unknown flag %q for \"ultra skill install\"\n", a)
				fmt.Fprintln(errW, "\nRun 'ultra skill install --help' for usage.")
				return 2
			}
			dir = a
		}
	}

	version, src, err := skillSource(dir)
	if err != nil {
		fmt.Fprintln(errW, "ultra skill install:", err)
		failVerdict(errW, "skill install", err.Error())
		return 1
	}
	dest := filepath.Join(dir, skillDest)
	metaPath := filepath.Join(dest, skillManifest)

	if check {
		b, err := os.ReadFile(metaPath)
		if err != nil {
			fmt.Fprintf(errW, "ultra skill install --check: no vendored skill at %s — run `ultra skill install`\n", dest)
			failVerdict(errW, "skill install --check", "nothing vendored")
			return 1
		}
		var meta skillMeta
		if err := json.Unmarshal(b, &meta); err != nil || meta.Version != version {
			fmt.Fprintf(errW, "ultra skill install --check: vendored skill is %s but go.mod pins %s — run `ultra skill install`\n", meta.Version, version)
			failVerdict(errW, "skill install --check", "stale: vendored "+meta.Version+", pinned "+version)
			return 1
		}
		fmt.Fprintf(out, "vendored skill matches go.mod (%s)\n", version)
		verdict(errW, "skill install --check", "vendored skill matches "+version)
		return 0
	}

	// A skill dir without our manifest is hand-managed — refuse politely.
	if _, err := os.Stat(dest); err == nil {
		if _, merr := os.Stat(metaPath); merr != nil && !force {
			fmt.Fprintf(errW, "ultra skill install: %s exists without %s (hand-managed?) — --force to replace it\n", dest, skillManifest)
			failVerdict(errW, "skill install", "refused: hand-managed skill dir")
			return 1
		}
		if err := os.RemoveAll(dest); err != nil {
			fmt.Fprintln(errW, err)
			failVerdict(errW, "skill install", err.Error())
			return 1
		}
	}

	n, err := copySkill(src, dest)
	if err != nil {
		fmt.Fprintln(errW, "ultra skill install:", err)
		failVerdict(errW, "skill install", err.Error())
		return 1
	}
	meta, _ := json.MarshalIndent(skillMeta{Version: version, Source: src}, "", "  ")
	if err := os.WriteFile(metaPath, append(meta, '\n'), 0o644); err != nil {
		fmt.Fprintln(errW, err)
		failVerdict(errW, "skill install", err.Error())
		return 1
	}
	fmt.Fprintf(out, "vendored the %s skill into %s (%d files)\n", version, dest, n)
	fmt.Fprintf(out, "agents working in this repo now read the doctrine for the EXACT framework version it pins;\n`ultra upgrade` refreshes it with the bump. Commit it: git add %s\n", skillDest)
	verdict(errW, "skill install", fmt.Sprintf("vendored %s (%s)", version, count(n, "file")))
	return 0
}

// skillSource resolves where to copy the skill from: an active replace
// directory first (live checkout), else the module cache for the pinned
// version (downloading it if absent).
func skillSource(dir string) (version, src string, err error) {
	const mod = "github.com/bronystylecrazy/ultrastack"
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", "", fmt.Errorf("no go.mod here — run inside a product (%w)", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		l := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(l, "replace "+mod+" "); ok && !strings.Contains(l, mod+"/") {
			parts := strings.Fields(rest) // "=> ../path" or "=> path v1.2.3"
			if len(parts) >= 2 && parts[0] == "=>" && strings.Contains(parts[1], string(os.PathSeparator)) || len(parts) >= 2 && parts[0] == "=>" && strings.HasPrefix(parts[1], ".") {
				p := parts[1]
				if !filepath.IsAbs(p) {
					p = filepath.Join(dir, p)
				}
				if _, err := os.Stat(filepath.Join(p, "SKILL.md")); err != nil {
					return "", "", fmt.Errorf("replace target %s has no SKILL.md", p)
				}
				return "replace:" + parts[1], p, nil
			}
		}
	}
	version = ""
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) >= 2 && f[0] == mod && strings.HasPrefix(f[1], "v") {
			version = f[1]
			break
		}
		if len(f) >= 3 && f[0] == "require" && f[1] == mod {
			version = f[2]
			break
		}
	}
	if version == "" {
		return "", "", fmt.Errorf("go.mod does not require %s", mod)
	}
	cache, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		return "", "", err
	}
	src = filepath.Join(strings.TrimSpace(string(cache)), mod+"@"+version)
	if _, err := os.Stat(filepath.Join(src, "SKILL.md")); err != nil {
		dl := exec.Command("go", "mod", "download", mod)
		dl.Dir = dir
		if out, derr := dl.CombinedOutput(); derr != nil {
			return "", "", fmt.Errorf("module %s@%s not in cache and download failed: %v\n%s", mod, version, derr, out)
		}
	}
	if _, err := os.Stat(filepath.Join(src, "SKILL.md")); err != nil {
		return "", "", fmt.Errorf("%s@%s has no SKILL.md (too old?)", mod, version)
	}
	return version, src, nil
}

// copySkill copies SKILL.md + references/ (the doctrine set — nothing else)
// into dest. Module-cache files are read-only; copies are made writable.
func copySkill(src, dest string) (int, error) {
	n := 0
	cp := func(rel string) error {
		return filepath.WalkDir(filepath.Join(src, rel), func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			r, err := filepath.Rel(src, p)
			if err != nil {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			target := filepath.Join(dest, r)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			n++
			return os.WriteFile(target, b, 0o644)
		})
	}
	if err := cp("SKILL.md"); err != nil {
		return n, err
	}
	if err := cp("references"); err != nil {
		return n, err
	}
	return n, nil
}
