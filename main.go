package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"github.com/ngicks/go-iterator-helper/hiter"
	"github.com/ngicks/go-iterator-helper/hiter/ioiter"
	"github.com/ngicks/go-iterator-helper/x/exp/xiter"
)

var (
	dir = flag.String("dir", "", "")
	v   = flag.Bool("v", false, "")
	f   = flag.Bool("f", false, "force option: ignores errors")
	n   = flag.String("new", "", "creates command sets for given name")
)

type namedCommandSet struct {
	Name string
	Set  commandSet
}

type commandSet struct {
	Ver         []string `json:"ver,omitzero"`
	CheckLatest []string `json:"checklatest,omitzero"`
	Install     []string `json:"install,omitzero"`
	Update      []string `json:"update,omitzero"`
}

type command string

const (
	commandVer         command = "ver"
	commandChecklatest command = "checklatest"
	commandInstall     command = "install"
	commandUpdate      command = "update"
)

var cmds = []command{commandVer, commandChecklatest, commandInstall, commandUpdate}

func (c commandSet) Select(kind command) []string {
	switch kind {
	default:
		panic(fmt.Errorf("unknown command: %q", kind))
	case commandVer:
		return c.Ver
	case commandChecklatest:
		return c.CheckLatest
	case commandInstall:
		return c.Install
	case commandUpdate:
		return c.Update
	}
}

type commandExecutor struct {
	dir        string
	commandSet namedCommandSet
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
}

func newCommandExecutor(
	dir string,
	commandSet namedCommandSet,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) *commandExecutor {
	return &commandExecutor{
		dir:        dir,
		commandSet: commandSet,
		stdin:      stdin,
		stdout:     stdout,
		stderr:     stderr,
	}
}

func (e commandExecutor) Exec(
	ctx context.Context,
	kind command,
	ver string,
	verbose bool,
) (string, error) {
	args := e.commandSet.Set.Select(kind)
	if len(args) > 0 {
		dict := dictReplacer{
			"${VER}":  ver,
			"${OS}":   runtime.GOOS,
			"${ARCH}": runtime.GOARCH,
		}
		args = slices.Collect(dict.Map(slices.Values(args)))
	} else {
		for _, suf := range []string{"", ".sh", ".exe", ".bat", ".ps1"} {
			name := filepath.Join(e.dir, e.commandSet.Name, string(kind)+suf)
			_, err := os.Stat(name)
			if err == nil {
				args = append(slices.Clip(args), name)
				break
			}
		}
		if len(args) == 0 {
			return "", fmt.Errorf("command not found")
		}
	}

	cmd := exec.CommandContext(ctx, args[0])
	if len(args) > 1 {
		cmd.Args = args
	}

	cmd.Stdin = e.stdin

	buf := new(bytes.Buffer)
	if !verbose {
		cmd.Stdout = buf
	} else {
		cmd.Stdout = io.MultiWriter(buf, e.stdout)
	}
	cmd.Stderr = e.stderr

	cmd.Env = append(os.Environ(), "OS="+runtime.GOOS, "ARCH="+runtime.GOARCH)
	if ver != "" {
		cmd.Env = append(cmd.Env, "VER="+ver)
	}

	err := cmd.Run()
	return buf.String(), err
}

const (
	pinnedVersionsFileName = ".pin.json"
)

func main() {
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfgDir := *dir

	if cfgDir == "" {
		userCfgDir, err := os.UserConfigDir()
		if err != nil {
			panic(fmt.Errorf("getting os.UserConfigDir: %w", err))
		}
		cfgDir = filepath.Join(userCfgDir, "ngpkgmgr")
	}

	if *n != "" {
		f, err := os.OpenFile(filepath.Join(cfgDir, *n+".json"), os.O_RDWR|os.O_CREATE|os.O_EXCL, fs.ModePerm)
		switch {
		default:
			panic(err)
		case errors.Is(err, fs.ErrExist):
		case err == nil:
			enc := json.NewEncoder(f)
			enc.SetIndent("", "    ")
			err := enc.Encode(commandSet{
				Ver:         []string{},
				Install:     []string{},
				CheckLatest: []string{},
				Update:      []string{},
			})
			_ = f.Close()
			if err != nil {
				panic(err)
			}
		}
		err = os.Mkdir(filepath.Join(cfgDir, *n), fs.ModePerm)
		if err != nil && !errors.Is(err, fs.ErrExist) {
			panic(err)
		}
		for _, c := range cmds {
			scriptName := filepath.Join(cfgDir, *n, string(c))
			switch runtime.GOOS {
			case "windows":
				scriptName += ".ps1"
			default:
				scriptName += ".sh"
			}
			f, err := os.OpenFile(scriptName, os.O_RDWR|os.O_CREATE|os.O_EXCL, fs.ModePerm)
			switch {
			default:
				panic(err)
			case errors.Is(err, fs.ErrExist):
			case err == nil:
				_, err := fmt.Fprintf(f, "#!%s\n", cmp.Or(os.Getenv("SHELL"), "/bin/bash"))
				_ = f.Close()
				if err != nil {
					panic(err)
				}
			}
		}
		return
	}

	var tgt, cmd string
	args := flag.Args()
	switch len(args) {
	case 2:
		tgt, cmd = args[0], args[1]
	case 1:
		cmd = args[0]
	default:
		panic(fmt.Errorf("wrong args length: want 2 or 1, got %d", len(args)))
	}

	if !slices.Contains(cmds, command(cmd)) {
		panic(fmt.Errorf("unknown command: must be one of %v", cmds))
	}

	pinnedVersions := map[string]string{}
	pinFile, err := os.Open(filepath.Join(cfgDir, pinnedVersionsFileName))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			panic(err)
		}
	} else {
		err = json.NewDecoder(pinFile).Decode(&pinnedVersions)
		_ = pinFile.Close()
		if err != nil {
			panic(err)
		}
	}

	for k, v := range pinnedVersions {
		if k != strings.TrimSpace(k) || v != strings.TrimSpace(v) {
			panic(fmt.Errorf("pinned version %q has space prefix and/or suffix in name or version", k))
		}
	}

	var sets []namedCommandSet
	if tgt != "" {
		f, err := os.Open(filepath.Join(cfgDir, tgt+".json"))
		if err == nil {
			var set commandSet
			err = json.NewDecoder(f).Decode(&set)
			_ = f.Close()
			if err != nil {
				panic(err)
			}
			sets = append(sets, namedCommandSet{Name: tgt, Set: set})
		} else if !errors.Is(err, fs.ErrNotExist) {
			panic(err)
		} else {
			s, err := os.Stat(filepath.Join(cfgDir, tgt))
			if err != nil {
				panic(err)
			}
			if !s.IsDir() {
				panic(fmt.Errorf("file %[1]q.json or directory %[1]q must exist", tgt))
			}
			sets = append(sets, namedCommandSet{Name: tgt})
		}
	} else {
		dir, err := os.Open(cfgDir)
		if err != nil {
			panic(err)
		}

		sets, err = hiter.TryAppendSeq(
			sets[:0],
			xiter.Map2(
				func(fi fs.FileInfo, err error) (namedCommandSet, error) {
					switch {
					default:
						return namedCommandSet{}, err
					case fi.Mode().IsRegular() && strings.HasSuffix(fi.Name(), ".json"):
						f, err := os.Open(filepath.Join(cfgDir, fi.Name()))
						if err != nil {
							return namedCommandSet{}, err
						}
						var set commandSet
						err = json.NewDecoder(f).Decode(&set)
						_ = f.Close()
						if err != nil {
							return namedCommandSet{}, err
						}
						return namedCommandSet{Name: strings.TrimSuffix(fi.Name(), ".json"), Set: set}, nil
					case fi.IsDir():
						// directory should contain scripts.
						return namedCommandSet{Name: fi.Name()}, nil
					}
				},
				xiter.Filter2(
					func(fi fs.FileInfo, err error) bool {
						switch {
						default:
							return false
						case err != nil,
							fi.Mode().IsRegular() && strings.HasSuffix(fi.Name(), ".json") && fi.Name() != pinnedVersionsFileName,
							fi.IsDir():
							return true
						}
					},
					ioiter.Readdir(dir),
				),
			),
		)
		_ = dir.Close()
		if err != nil {
			panic(err)
		}
		slices.SortFunc(
			sets,
			func(i, j namedCommandSet) int {
				if c := cmp.Compare(i.Name, j.Name); c != 0 {
					return c
				}
				switch {
				case reflect.ValueOf(i.Set).IsZero():
					// x > y
					return +1
				case reflect.ValueOf(j.Set).IsZero():
					return -1
				default:
					return 0
				}
			},
		)
		// may contain both .json and directory
		sets = slices.CompactFunc(sets, func(i, j namedCommandSet) bool { return i.Name == j.Name })
	}

	currentVersions := map[string]string{}
	latestVersions := map[string]string{}

	iter := func() iter.Seq[*commandExecutor] {
		return func(yield func(*commandExecutor) bool) {
			for _, set := range sets {
				executor := newCommandExecutor(cfgDir, set, os.Stdin, os.Stdout, os.Stderr)
				if !yield(executor) {
					return
				}
			}
		}
	}

	switch command(cmd) {
	case commandInstall:
		for executor := range iter() {
			fmt.Printf("installing %q...\n\n", executor.commandSet.Name)
			out, err := executor.Exec(ctx, commandVer, "", false)
			if err == nil {
				fmt.Printf("Skipping %q: seems already installed at version %s\n", executor.commandSet.Name, strings.TrimSpace(out))
				continue
			}

			out, err = executor.Exec(ctx, commandChecklatest, "", false)
			ver := strings.TrimSpace(out)
			if err != nil {
				ver = ""
				fmt.Printf("\nfetching latest version failed with err %v\nNow trying with no version specified\n", err)
			}

			_, err = executor.Exec(ctx, commandInstall, cmp.Or(pinnedVersions[executor.commandSet.Name], ver), *v)
			if err != nil {
				err := fmt.Errorf("install %q: %w", executor.commandSet.Name, err)
				if !*f {
					panic(err)
				}
				fmt.Printf("warn: failed: %v\n", err)
			} else {
				fmt.Printf("\n\ninstalling %q done!\n", executor.commandSet.Name)
			}
		}
	case commandVer:
		for executor := range iter() {
			out, err := executor.Exec(ctx, commandVer, "", false)
			if err != nil {
				err := fmt.Errorf("ver %q: %w", executor.commandSet.Name, err)
				if !*f {
					panic(err)
				}
				fmt.Printf("warn: failed: %v\n", err)
			}
			currentVersions[executor.commandSet.Name] = strings.TrimSpace(out)
		}
		fmt.Printf("%s\n", must(json.MarshalIndent(currentVersions, "", "    ")))
	case commandChecklatest:
		for executor := range iter() {
			out, err := executor.Exec(ctx, commandChecklatest, "", false)
			if err != nil {
				err := fmt.Errorf("checklatest %q: %w", executor.commandSet.Name, err)
				if !*f {
					panic(err)
				}
				fmt.Printf("warn: failed: %v\n", err)
			}
			latestVersions[executor.commandSet.Name] = strings.TrimSpace(out)
		}
		fmt.Printf("%s\n", must(json.MarshalIndent(latestVersions, "", "    ")))
	case commandUpdate:
		for executor := range iter() {
			func() {
				out, err := executor.Exec(ctx, commandVer, "", *v)
				if err != nil {
					err := fmt.Errorf("ver %q: %w", executor.commandSet.Name, err)
					panic(err)
				}
				currentVersions[executor.commandSet.Name] = strings.TrimSpace(out)
			}()
			func() {
				out, err := executor.Exec(ctx, commandChecklatest, "", *v)
				if err != nil {
					err := fmt.Errorf("checklatest %q: %w", executor.commandSet.Name, err)
					panic(err)
				}
				latestVersions[executor.commandSet.Name] = strings.TrimSpace(out)
			}()
		}

		type targetedExecutor struct {
			tgt      string
			executor *commandExecutor
		}
		var updates []targetedExecutor
		for executor := range iter() {
			name := executor.commandSet.Name
			tgt := cmp.Or(pinnedVersions[name], latestVersions[name])
			fmt.Printf("%q: %s -> %s", name, currentVersions[name], tgt)
			if pinnedVersions[name] != "" {
				fmt.Printf("(pinned)")
			}
			if currentVersions[name] == tgt {
				fmt.Printf(": no update\n")
				continue
			}
			updates = append(updates, targetedExecutor{tgt: tgt, executor: executor})
			fmt.Printf("\n")
		}

		for _, t := range updates {
			fmt.Printf("updating %q...\n\n", t.executor.commandSet.Name)
			_, err := t.executor.Exec(ctx, commandUpdate, t.tgt, *v)
			if err != nil {
				panic(fmt.Errorf("updating %q: %w", t.executor.commandSet.Name, err))
			}
			fmt.Printf("\n\nupdated %q!\n", t.executor.commandSet.Name)
		}
	}
}

func must[V any](v V, err error) V {
	if err != nil {
		panic(err)
	}
	return v
}
