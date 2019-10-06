package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mitchellh/go-wordwrap"
	"github.com/peterbourgon/ff"
	"github.com/peterbourgon/ff/ffcli"
	"go.opencensus.io/exporter/jaeger"
	"go.opencensus.io/trace"

	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
)

func main() {
	var (
		gfs         = flag.NewFlagSet("mb", flag.ExitOnError)
		commitRange = gfs.String("commit-range", "", "Will be used as `git diff --name-only [commit-range]` to find file changes")
		configFile  = gfs.String("config", "./monobuild.yaml", "mb config file")
		diffOnly    = gfs.Bool("diff-only", false, "View changes without building")
		// TODO - put this on another command called 'mb trace'
		jaegerTrace       = gfs.Bool("trace", false, "Debug monobuild with Jaeger tracing")
		jaegerAgentEp     = gfs.String("trace-jaeger-agent", "localhost:6831", "Jaeger agent endpoint")
		jaegerCollectorEp = gfs.String("trace-jaeger-collector", "http://localhost:14268/api/traces", "jaeger collector endpoint API URI.")
	)
	root := &ffcli.Command{
		Usage:   "mb [flags]",
		FlagSet: gfs,
		Options: []ff.Option{ff.WithEnvVarPrefix("MB")},
		LongHelp: collapse(`
			mb is a build tool for Go monorepos.
		`, 80),
		Exec: func([]string) error {
			if *jaegerTrace {
				fmt.Println("Tracing is enabled.")
				je, err := jaeger.NewExporter(jaeger.Options{
					AgentEndpoint:     *jaegerAgentEp,
					CollectorEndpoint: *jaegerCollectorEp,
					ServiceName:       "mb-cli",
				})
				if err != nil {
					errors.Errorf("failed to create the Jaeger exporter: %v", err)
				}
				trace.RegisterExporter(je)
				trace.ApplyConfig(trace.Config{DefaultSampler: trace.AlwaysSample()})
			}
			ctx := context.Background()
			ctx, span := trace.StartSpan(ctx, "ffcli.Command.Exec()")
			defer span.End()

			b, err := NewBuildContext(ctx, *configFile, *commitRange)
			if err != nil {
				return err
			}
			if err := b.Diff(ctx); err != nil {
				return err
			}
			// TODO - pretty print the diff here.
			fmt.Println("Diff()")
			fmt.Println(b)
			if *diffOnly {
				fmt.Println("diff only")
				return nil
			}
			return b.MonoBuild(ctx)
		},
	}
	err := root.Run(os.Args[1:])
	if *jaegerTrace {
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		errfatal(err)
	}
}

func NewBuildContext(ctx context.Context, configFile, commitRange string) (*BuildContext, error) {
	ctx, span := trace.StartSpan(ctx, "NewBuildContext")
	defer span.End()
	b := &BuildContext{
		CommitRange: commitRange,
		ConfigFile:  configFile,
	}
	// Parse the config file.
	fb, err := ioutil.ReadFile(b.ConfigFile)
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(fb, &b.Config); err != nil {
		return nil, err
	}
	// Validate the config file.
	if err := b.Config.validate(ctx); err != nil {
		return nil, err
	}
	// Parse each target Go dependencies and watched files.
	for i := range b.Config.Targets {
		if err := b.Config.Targets[i].parseGoDeps(ctx); err != nil {
			return nil, err
		}
		if err := b.Config.Targets[i].parseWatchedFiles(ctx); err != nil {
			return nil, err
		}
	}
	span.AddAttributes(trace.StringAttribute("build_context", b.String()))
	return b, nil
}

// BuildContext represents a monobuild execution context.
type BuildContext struct {
	Config      Config
	Files       []*File
	ConfigFile  string
	CommitRange string
}

func (b *BuildContext) String() string {
	bc, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(bc)
}

func (b *BuildContext) Diff(ctx context.Context) error {
	ctx, span := trace.StartSpan(ctx, "*BuildContext.Diff()")
	defer span.End()
	// TODO - use go-git package!
	cmd := &exec.Cmd{}
	if b.CommitRange == "" {
		cmd = exec.CommandContext(ctx, "git", "diff", "--name-only")
	} else {
		cmd = exec.CommandContext(ctx, "git", "diff", "--name-only", b.CommitRange)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.Errorf(string(out))
	}
	files := strings.Split(string(out), "\n")
	for _, f := range files {
		// TODO - remove blank files from git diff
		if f == "" {
			continue
		}
		info, err := os.Stat(f)
		if err != nil {
			panic(err) // The file from git diff should always exists!
		}
		cf := &File{
			Name:     f,
			FileInfo: info,
		}
		// TODO change to BuildContext is not applied after this function..
		for _, t := range b.Config.Targets {
			if isFileDependencyOfTarget(f, t, b.Config.DepSourceDirs) {
				cf.DependencyOf = append(cf.DependencyOf, t.Path)
				t.Changes = append(t.Changes, cf)
				fmt.Printf("file %s is dependency of target %s\n", f, t.Path)
			}
			if isFileWatchedByTarget(f, t) {
				cf.WatchedBy = append(cf.WatchedBy, t.Path)
				t.Changes = append(t.Changes, cf)
				fmt.Printf("file %s is watched by target %s\n", f, t.Path)
			}
		}
		b.Files = append(b.Files, cf)
		fmt.Printf("file %s added to b.Files\n", f)
	}
	// DEBUG
	for _, bf := range b.Files {
		fmt.Println(bf)
	}
	span.AddAttributes(trace.StringAttribute("build_context", b.String()))
	return nil
}

func isFileWatchedByTarget(f string, t *Target) bool {
	for _, wf := range t.Watches {
		if f == wf {
			return true
		}
	}
	return false
}

func isFileDependencyOfTarget(f string, t *Target, depDirs []string) bool {
	if t.Deps == nil {
		return false
	}
	fdir := filepath.Dir(f)
	for _, depDir := range depDirs {
		// If the changed file has a prefix of any of the defined package directory,
		// then the changed file is identified as a dependency.
		if strings.HasPrefix(f, depDir) {
			// Check if any of the Target's dependency matches it.
			for _, dep := range t.Deps {
				if strings.Contains(dep, fdir) {
					return true
				}
			}
		}
	}
	return false
}

// File represents a file from the git diff command.
type File struct {
	Name         string
	DependencyOf []string
	WatchedBy    []string
	os.FileInfo  `json:"-"`
}

func (f *File) String() string {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Config represents the mb config file.
type Config struct {
	DepSourceDirs []string  `yaml:"dep_source_dirs"`
	Targets       []*Target `yaml:"targets"`
}

func (c *Config) validate(ctx context.Context) error {
	_, span := trace.StartSpan(ctx, "*Config.validate()")
	defer span.End()

	for _, f := range c.DepSourceDirs {
		finfo, err := os.Stat(f)
		if os.IsNotExist(err) {
			return err
		}
		if !finfo.IsDir() {
			return errors.Errorf("dep_source_dir: %s is not a directory", f)
		}
	}
	checkdup := make(map[string]int)
	for _, t := range c.Targets {
		if _, found := checkdup[t.Path]; found {
			return errors.Errorf("target.path: %s has been used more than once", t.Path)
		}
		finfo, err := os.Stat(t.Path)
		if os.IsNotExist(err) {
			return err
		}
		if !finfo.IsDir() {
			return errors.Errorf("target.path: %s is not a directory", t.Path)
		}
		checkdup[t.Path]++
	}
	return nil
}

// Target represents the target config.
type Target struct {
	Path         string       `yaml:"path"`
	BuildCommand BuildCommand `yaml:"build_command"`
	WatchPattern []string     `yaml:"watch_pattern"` // Any file that are considered as a dependency of the target.
	Dir          string       `json:"Dir"`           // This will be populated by go list.
	Deps         []string     `json:"Deps"`          // This will be populated by go list.
	Watches      []string     // This will be populated after parsing WatchPattern.
	Changes      []*File      // This will be populated after git diff.
}

func (c *Config) String() string {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (t *Target) String() string {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(b)
}

// BuildCommand  represents the build_command config.
type BuildCommand struct {
	Dir     string   `yaml:"dir"`
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	Output  string
	Error   string
}

var noTarget = errors.Errorf("no monobuild targets found")

func (b *BuildContext) MonoBuild(ctx context.Context) error {
	ctx, span := trace.StartSpan(ctx, "*BuildContext.MonoBuild()")
	defer span.End()
	span.AddAttributes(trace.StringAttribute("build_context", b.String()))
	if len(b.Config.Targets) == 0 {
		return noTarget
	}
	for _, t := range b.Config.Targets {
		// TODO - Prettify the print with debug mode
		if len(t.Changes) == 0 {
			fmt.Println("SKIPPING BUILD TARGET: ", t.Path)
			continue
		}
		fmt.Println("-------------------------------")
		fmt.Println("BUILDING TARGET: ", t.Path)
		fmt.Println(t.String())
		fmt.Println("-------------------------------")
		if err := t.Run(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (t *Target) parseWatchedFiles(ctx context.Context) error {
	_, span := trace.StartSpan(ctx, "*Target.parseWatchedFiles")
	defer span.End()
	var err error
	for _, p := range t.WatchPattern {
		t.Watches, err = filepath.Glob(p)
		if err != nil {
			return errors.Errorf("problem with target %s watch %s", t.Path, p)
		}
	}
	span.AddAttributes(trace.StringAttribute("target", t.String()))
	return nil
}

func (t *Target) parseGoDeps(ctx context.Context) error {
	_, span := trace.StartSpan(ctx, "*Target.parseGoDeps")
	defer span.End()
	// Add the dot slash prefix which is required for the `go list` command.
	dir := t.Path
	if !strings.HasPrefix(dir, "./") {
		dir = "./" + dir
	}
	cmd := exec.Command("go", "list", "-json", dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.Errorf("go list -json %s: %s", dir, string(out))
	}
	if err := json.Unmarshal(out, t); err != nil {
		panic(err)
	}
	span.AddAttributes(trace.StringAttribute("target", t.String()))
	return nil
}

func (t *Target) Run(ctx context.Context) error {
	ctx, span := trace.StartSpan(ctx, "*Target.Run()")
	defer span.End()
	defer func() {
		span.AddAttributes(trace.StringAttribute("target", t.String()))
	}()

	cmd := &exec.Cmd{}
	if len(t.BuildCommand.Args) > 0 {
		cmd = exec.CommandContext(ctx, t.BuildCommand.Command, t.BuildCommand.Args...)
	} else {
		cmd = exec.CommandContext(ctx, t.BuildCommand.Command)
	}
	// Set the command working directory.
	if t.BuildCommand.Dir != "" {
		if _, err := os.Stat(t.BuildCommand.Dir); os.IsNotExist(err) {
			return errors.Errorf("build command error: %s", err)
		}
		cmd.Dir = t.BuildCommand.Dir
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	stdoutIn, _ := cmd.StdoutPipe()
	stderrIn, _ := cmd.StderrPipe()
	stdout := io.MultiWriter(os.Stdout, &stdoutBuf)
	stderr := io.MultiWriter(os.Stderr, &stderrBuf)
	err := cmd.Start()
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		_, err := io.Copy(stdout, stdoutIn)
		if err != nil {
			panic(err)
		}
		wg.Done()
	}()

	_, err = io.Copy(stderr, stderrIn)
	if err != nil {
		panic(err)
	}
	wg.Wait()

	// Save the stdout and error for testing purposes.
	t.BuildCommand.Output = string(stdoutBuf.Bytes())
	t.BuildCommand.Error = string(stderrBuf.Bytes())

	err = cmd.Wait()
	if err != nil {
		return err
	}
	return nil
}

func collapse(body string, width uint) string {
	var b strings.Builder
	s := bufio.NewScanner(strings.NewReader(body))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		b.WriteString(line + " ")
	}
	return wordwrap.WrapString(b.String(), width)
}

func errfatal(m interface{}) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", m)
	os.Exit(1)
}
