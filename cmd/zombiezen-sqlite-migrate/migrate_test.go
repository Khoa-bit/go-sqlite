// Copyright 2021 Roxy Light
// SPDX-License-Identifier: ISC

package main

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/tools/go/packages"
)

func TestProcess(t *testing.T) {
	crawshawModule, err := skeletonModule("crawshaw.io/sqlite")
	if err != nil {
		t.Fatal(err)
	}
	bassModule, err := skeletonModule("zombiezen.com/go/bass")
	if err != nil {
		t.Fatal(err)
	}
	sqliteModule, err := skeletonModule("zombiezen.com/go/sqlite")
	if err != nil {
		t.Fatal(err)
	}
	rootDir := filepath.Join("testdata", "TestProcess")
	testRunContents, err := os.ReadDir(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	const mainPkgPath = "example.com/foo"
	const mainFilename = "file.go"
	for _, ent := range testRunContents {
		name := ent.Name()
		if !ent.IsDir() || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(rootDir, name)
			want, err := os.ReadFile(filepath.Join(dir, "want.go"))
			if err != nil {
				t.Fatal(err)
			}
			want, err = format.Source(want)
			if err != nil {
				t.Fatal(err)
			}

			originalFile := filepath.Join(dir, "original.go")
			cfg, err := exportModules(t, []testModule{
				crawshawModule,
				bassModule,
				sqliteModule,
				{
					Name: mainPkgPath,
					Files: map[string]string{
						mainFilename: originalFile,
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			cfg.Mode = processMode
			pkgs, err := packages.Load(cfg, "pattern="+mainPkgPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(pkgs) != 1 {
				t.Fatalf("Found %d packages; want 1", len(pkgs))
			}
			pkg := pkgs[0]
			if len(pkg.Errors) > 0 {
				for _, err := range pkg.Errors {
					t.Errorf("Load %s: %v", originalFile, err)
				}
				return
			}
			if len(pkg.Syntax) != 1 {
				t.Fatalf("Found %d parsed files; want 1", len(pkg.Syntax))
			}
			file := pkg.Syntax[0]

			for _, err := range process(pkg, file) {
				t.Logf("process: %v", err)
			}

			got, err := formatFile(new(bytes.Buffer), originalFile, pkg.Fset, file)
			if err != nil {
				t.Fatalf("Formatting output: %v", err)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("diff a/%s b/%s:\n%s", mainFilename, mainFilename, diff)
			}
		})
	}
}

type testModule struct {
	Name  string
	Files map[string]string
}

func exportModules(t *testing.T, modules []testModule) (*packages.Config, error) {
	t.Helper()
	if len(modules) == 0 {
		return nil, fmt.Errorf("no modules to export")
	}
	rootDir := t.TempDir()
	mainModule := modules[len(modules)-1]
	mainDir := filepath.Join(rootDir, "main")
	if err := writeTestModule(mainDir, mainModule); err != nil {
		return nil, err
	}

	var goMod strings.Builder
	fmt.Fprintf(&goMod, "module %s\n\ngo 1.20\n\nrequire (\n", mainModule.Name)
	for _, mod := range modules[:len(modules)-1] {
		fmt.Fprintf(&goMod, "\t%s v0.0.0\n", mod.Name)
	}
	goMod.WriteString(")\n\n")
	for i, mod := range modules[:len(modules)-1] {
		fmt.Fprintf(&goMod, "replace %s => ../module-%d\n", mod.Name, i)
	}
	if err := os.WriteFile(filepath.Join(mainDir, "go.mod"), []byte(goMod.String()), 0644); err != nil {
		return nil, err
	}
	for i, mod := range modules[:len(modules)-1] {
		if err := writeTestModule(filepath.Join(rootDir, fmt.Sprintf("module-%d", i)), mod); err != nil {
			return nil, err
		}
	}

	env := make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(item), "GOWORK=") {
			env = append(env, item)
		}
	}
	env = append(env, "GOWORK=off")
	return &packages.Config{Dir: mainDir, Env: env}, nil
}

func writeTestModule(dir string, mod testModule) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	goMod := fmt.Sprintf("module %s\n\ngo 1.20\n", mod.Name)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
		return err
	}
	for relPath, sourcePath := range mod.Files {
		data, err := os.ReadFile(sourcePath)
		if err != nil {
			return fmt.Errorf("read module %q file %q: %w", mod.Name, relPath, err)
		}
		dst := filepath.Join(dir, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0644); err != nil {
			return err
		}
	}
	return nil
}

func skeletonModule(importPath string) (testModule, error) {
	mod := testModule{
		Name:  importPath,
		Files: make(map[string]string),
	}
	dir := filepath.Join("testdata", "skeleton", filepath.FromSlash(importPath))
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		relpath := strings.TrimPrefix(path, dir+string(filepath.Separator))
		mod.Files[filepath.ToSlash(relpath)] = path
		return nil
	})
	if err != nil {
		return testModule{}, fmt.Errorf("load skeleton module %q: %w", importPath, err)
	}
	return mod, nil
}

type logWriter struct {
	logger interface{ Logf(string, ...any) }
	buf    []byte
}

func (lw *logWriter) Write(p []byte) (int, error) {
	lastLF := bytes.LastIndexByte(p, '\n')
	if lastLF == -1 {
		lw.buf = append(lw.buf, p...)
		return len(p), nil
	}
	if len(lw.buf) > 0 {
		lw.buf = append(lw.buf, p[:lastLF]...)
		lw.logger.Logf("%s", lw.buf)
		lw.buf = lw.buf[:0]
	} else {
		lw.logger.Logf("%s", p[:lastLF])
	}
	lw.buf = append(lw.buf, p[lastLF+1:]...)
	return len(p), nil
}
