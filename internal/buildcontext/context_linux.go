//go:build linux

// Package buildcontext plans and streams bounded, identity-checked build tars.
package buildcontext

import (
	"archive/tar"
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	DefaultMaxBytes = int64(2 << 30)
	DefaultMaxFiles = 200_000
)

type Options struct {
	PermittedRoots []string
	Containerfile  string
	MaxBytes       int64
	MaxFiles       int
	CohotfsRoot    string
}

type Entry struct {
	Path       string      `json:"path"`
	Size       int64       `json:"size"`
	Mode       os.FileMode `json:"mode"`
	LinkTarget string      `json:"linkTarget,omitempty"`
	Device     uint64      `json:"-"`
	Inode      uint64      `json:"-"`
	ChangeTime int64       `json:"-"`
}

type Plan struct {
	Root       string  `json:"root"`
	Entries    []Entry `json:"entries"`
	TotalBytes int64   `json:"totalBytes"`
}

func BuildPlan(contextPath string, options Options) (Plan, error) {
	if options.MaxBytes == 0 {
		options.MaxBytes = DefaultMaxBytes
	}
	if options.MaxFiles == 0 {
		options.MaxFiles = DefaultMaxFiles
	}
	canonical, err := filepath.Abs(contextPath)
	if err != nil {
		return Plan{}, err
	}
	canonical, err = filepath.EvalSymlinks(canonical)
	if err != nil {
		return Plan{}, err
	}
	if err := allowed(canonical, options.PermittedRoots); err != nil {
		return Plan{}, err
	}
	if options.CohotfsRoot != "" && isBeneath(canonical, options.CohotfsRoot) {
		return Plan{}, fmt.Errorf("build context cannot be inside the Cohotfs local root")
	}
	patterns, err := loadIgnorePatterns(canonical)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{Root: canonical}
	err = filepath.WalkDir(canonical, func(path string, directory os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == canonical {
			return nil
		}
		rel, err := filepath.Rel(canonical, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if defaultSecret(rel) {
			if directory.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		ignored := patterns.ignored(rel, directory.IsDir())
		if rel == ".dockerignore" || rel == filepath.ToSlash(options.Containerfile) {
			ignored = false
		}
		if ignored {
			if directory.IsDir() && !patterns.mightReinclude(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := directory.Info()
		if err != nil {
			return err
		}
		entry := Entry{Path: rel, Size: info.Size(), Mode: info.Mode()}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("filesystem identity unavailable for %s", rel)
		}
		entry.Device, entry.Inode, entry.ChangeTime = uint64(stat.Dev), stat.Ino, stat.Ctim.Sec*1_000_000_000+stat.Ctim.Nsec
		switch {
		case info.Mode().IsRegular():
			plan.TotalBytes += info.Size()
			if plan.TotalBytes > options.MaxBytes {
				return fmt.Errorf("build context exceeds %d bytes", options.MaxBytes)
			}
		case info.IsDir():
			entry.Size = 0
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			resolved := target
			if !filepath.IsAbs(target) {
				resolved = filepath.Join(filepath.Dir(path), target)
			}
			resolved, err = filepath.EvalSymlinks(resolved)
			if err != nil || !isBeneath(resolved, canonical) {
				return fmt.Errorf("symlink %s escapes build context", rel)
			}
			entry.LinkTarget = target
			entry.Size = 0
		default:
			return fmt.Errorf("unsupported socket/device/fifo in build context: %s", rel)
		}
		plan.Entries = append(plan.Entries, entry)
		if len(plan.Entries) > options.MaxFiles {
			return fmt.Errorf("build context exceeds %d entries", options.MaxFiles)
		}
		return nil
	})
	if err != nil {
		return Plan{}, err
	}
	sort.Slice(plan.Entries, func(i, j int) bool { return plan.Entries[i].Path < plan.Entries[j].Path })
	return plan, nil
}

func (p Plan) WriteTar(writer io.Writer) error {
	rootFD, err := unix.Open(p.Root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(rootFD)
	tarWriter := tar.NewWriter(writer)
	for _, entry := range p.Entries {
		info := syntheticInfo{entry: entry}
		header, err := tar.FileInfoHeader(info, entry.LinkTarget)
		if err != nil {
			return err
		}
		header.Name = entry.Path
		header.Uid, header.Gid = 0, 0
		header.Uname, header.Gname = "", ""
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		if !entry.Mode.IsRegular() {
			continue
		}
		fd, err := unix.Openat2(rootFD, entry.Path, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS})
		if err != nil {
			return err
		}
		file := os.NewFile(uintptr(fd), entry.Path)
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil || uint64(stat.Dev) != entry.Device || stat.Ino != entry.Inode || stat.Size != entry.Size || stat.Ctim.Sec*1_000_000_000+stat.Ctim.Nsec != entry.ChangeTime {
			_ = file.Close()
			return fmt.Errorf("build context entry changed after planning: %s", entry.Path)
		}
		if _, err := io.CopyN(tarWriter, file, entry.Size); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return tarWriter.Close()
}

func allowed(candidate string, roots []string) error {
	for _, root := range roots {
		canonical, err := filepath.EvalSymlinks(root)
		if err == nil && isBeneath(candidate, canonical) {
			return nil
		}
	}
	return fmt.Errorf("build context is outside permitted roots")
}

func isBeneath(candidate, root string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func defaultSecret(path string) bool {
	base := filepath.Base(path)
	for _, component := range strings.Split(filepath.ToSlash(path), "/") {
		if component == ".ssh" || component == ".gnupg" {
			return true
		}
	}
	if base == ".env" || strings.HasPrefix(base, ".env.") || base == "auth.json" || base == "agent.db" || base == ".claude.json" || base == "credentials" {
		return true
	}
	return strings.HasPrefix(base, "id_") && !strings.HasSuffix(base, ".pub")
}

type ignorePattern struct {
	negate    bool
	directory bool
	hasSlash  bool
	regex     *regexp.Regexp
	raw       string
}

type ignorePatterns []ignorePattern

func loadIgnorePatterns(root string) (ignorePatterns, error) {
	file, err := os.Open(filepath.Join(root, ".dockerignore"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var patterns ignorePatterns
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pattern := ignorePattern{raw: line}
		if strings.HasPrefix(line, "!") {
			pattern.negate = true
			line = strings.TrimPrefix(line, "!")
		}
		line = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(line)), "/")
		pattern.directory = strings.HasSuffix(scanner.Text(), "/")
		pattern.hasSlash = strings.Contains(line, "/")
		compiled, err := compilePattern(line, pattern.hasSlash)
		if err != nil {
			return nil, err
		}
		pattern.regex = compiled
		patterns = append(patterns, pattern)
	}
	return patterns, scanner.Err()
}

func (patterns ignorePatterns) ignored(path string, directory bool) bool {
	ignored := false
	for _, pattern := range patterns {
		candidate := path
		if !pattern.hasSlash {
			candidate = filepath.Base(path)
		}
		if pattern.regex.MatchString(candidate) || (pattern.directory && strings.HasPrefix(path, strings.TrimSuffix(pattern.raw, "/")+"/")) {
			ignored = !pattern.negate
		}
	}
	return ignored
}

func (patterns ignorePatterns) mightReinclude(directory string) bool {
	for _, pattern := range patterns {
		if pattern.negate && strings.Contains(pattern.raw, directory) {
			return true
		}
	}
	return false
}

func compilePattern(pattern string, slash bool) (*regexp.Regexp, error) {
	var expression strings.Builder
	if slash {
		expression.WriteString("^")
	} else {
		expression.WriteString("^")
	}
	for index := 0; index < len(pattern); {
		switch pattern[index] {
		case '*':
			if index+1 < len(pattern) && pattern[index+1] == '*' {
				expression.WriteString(".*")
				index += 2
			} else {
				expression.WriteString("[^/]*")
				index++
			}
		case '?':
			expression.WriteString("[^/]")
			index++
		default:
			expression.WriteString(regexp.QuoteMeta(string(pattern[index])))
			index++
		}
	}
	expression.WriteString("$")
	return regexp.Compile(expression.String())
}

type syntheticInfo struct{ entry Entry }

func (i syntheticInfo) Name() string       { return filepath.Base(i.entry.Path) }
func (i syntheticInfo) Size() int64        { return i.entry.Size }
func (i syntheticInfo) Mode() os.FileMode  { return i.entry.Mode }
func (i syntheticInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (i syntheticInfo) IsDir() bool        { return i.entry.Mode.IsDir() }
func (i syntheticInfo) Sys() any           { return nil }
