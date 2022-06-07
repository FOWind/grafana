package codegen

import (
	"bytes"
	gerrors "errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"text/template"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/build"
	"cuelang.org/go/cue/cuecontext"
	"cuelang.org/go/cue/errors"
	"cuelang.org/go/cue/load"
	"cuelang.org/go/cue/parser"
	"github.com/grafana/cuetsy"
	"github.com/grafana/grafana"
	"github.com/grafana/thema"
	tload "github.com/grafana/thema/load"
)

// The only import statement we currently allow in any models.cue file
const schemasPath = "github.com/grafana/grafana/packages/grafana-schema/src/schema"

// CUE import paths, mapped to corresponding TS import paths. An empty value
// indicates the import path should be dropped in the conversion to TS. Imports
// not present in the list are not not allowed, and code generation will fail.
var importMap = map[string]string{
	"github.com/grafana/thema": "",
	schemasPath:                "@grafana/schema",
}

// Hard-coded list of paths to skip. Remove a particular file as we're ready
// to rely on the TypeScript auto-generated by cuetsy for that particular file.
var skipPaths = []string{
	"public/app/plugins/panel/barchart/models.cue",
	"public/app/plugins/panel/canvas/models.cue",
	"public/app/plugins/panel/histogram/models.cue",
	"public/app/plugins/panel/heatmap-new/models.cue",
	"public/app/plugins/panel/candlestick/models.cue",
	"public/app/plugins/panel/state-timeline/models.cue",
	"public/app/plugins/panel/status-history/models.cue",
	"public/app/plugins/panel/table/models.cue",
	"public/app/plugins/panel/timeseries/models.cue",
}

const prefix = "/"

// CuetsifyPlugins runs cuetsy against plugins' models.cue files.
func CuetsifyPlugins(ctx *cue.Context, root string) (WriteDiffer, error) {
	lib := thema.NewLibrary(ctx)
	// TODO this whole func has a lot of old, crufty behavior from the scuemata era; needs TLC
	overlay := make(map[string]load.Source)
	err := toOverlay(prefix, grafana.CueSchemaFS, overlay)
	// err := tload.ToOverlay(prefix, grafana.CueSchemaFS, overlay)
	if err != nil {
		return nil, err
	}

	exclude := func(path string) bool {
		for _, p := range skipPaths {
			if path == p {
				return true
			}
		}

		return filepath.Dir(path) == "cue.mod"
	}

	// Prep the cue load config
	clcfg := &load.Config{
		Overlay: overlay,
		// FIXME these module paths won't work for things not under our cue.mod - AKA third-party plugins
		ModuleRoot: prefix,
		Module:     "github.com/grafana/grafana",
	}

	outfiles := NewWriteDiffer()

	cuetsify := func(in fs.FS) error {
		seen := make(map[string]bool)
		return fs.WalkDir(in, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			dir := filepath.Dir(path)

			if d.IsDir() || filepath.Ext(d.Name()) != ".cue" || seen[dir] || exclude(path) {
				return nil
			}
			seen[dir] = true
			clcfg.Dir = filepath.Join(root, dir)

			var b []byte
			f := &tsFile{}

			switch {
			default:
				insts := load.Instances(nil, clcfg)
				if len(insts) > 1 {
					return fmt.Errorf("%s: resulted in more than one instance", path)
				}
				v := ctx.BuildInstance(insts[0])

				b, err = cuetsy.Generate(v, cuetsy.Config{})
				if err != nil {
					return err
				}

			case strings.Contains(path, "public/app/plugins"): // panel plugin models.cue files
				// The simple - and preferable - thing would be to have plugins use the same
				// package name for their models.cue as their containing dir. That's not
				// possible, though, because we allow dashes in plugin names, but CUE does not
				// allow them in package names. Yuck.
				inst, err := loadInstancesWithThema(in, dir, "grafanaschema")
				if err != nil {
					return fmt.Errorf("could not load CUE instance for %s: %w", dir, err)
				}

				// Also parse file directly to extract imports.
				// NOTE this will need refactoring to support working with more than one file at a time
				of, _ := in.Open(path)
				pf, _ := parser.ParseFile(filepath.Base(path), of, parser.ParseComments)

				iseen := make(map[string]bool)
				for _, im := range pf.Imports {
					ip := strings.Trim(im.Path.Value, "\"")
					mappath, has := importMap[ip]
					if !has {
						// TODO make a specific error type for this
						var all []string
						for im := range importMap {
							all = append(all, fmt.Sprintf("\t%s", im))
						}
						return errors.Newf(im.Pos(), "%s: import %q not allowed, panel plugins may only import from:\n%s\n", path, ip, strings.Join(all, "\n"))
					}
					// TODO this approach will silently swallow the unfixable
					// error case where multiple files in the same dir import
					// the same package to a different ident
					if mappath != "" && !iseen[ip] {
						iseen[ip] = true
						f.Imports = append(f.Imports, convertImport(im))
					}
				}

				v := ctx.BuildInstance(inst)

				lin, err := thema.BindLineage(v.LookupPath(cue.ParsePath("Panel")), lib)
				if err != nil {
					return fmt.Errorf("%s: failed to bind lineage: %w", path, err)
				}
				f.V = thema.LatestVersion(lin)
				f.WriteModelVersion = true

				b, err = cuetsy.Generate(thema.SchemaP(lin, f.V).UnwrapCUE(), cuetsy.Config{})
				if err != nil {
					return err
				}
			}

			f.Body = string(b)

			var buf bytes.Buffer
			err = tsTemplate.Execute(&buf, f)
			outfiles[filepath.Join(root, strings.Replace(path, ".cue", ".gen.ts", -1))] = buf.Bytes()
			return err
		})
	}

	err = cuetsify(grafana.CueSchemaFS)
	if err != nil {
		return nil, gerrors.New(errors.Details(err, nil))
	}

	return outfiles, nil
}

func convertImport(im *ast.ImportSpec) *tsImport {
	tsim := &tsImport{
		Pkg: importMap[schemasPath],
	}
	if im.Name != nil && im.Name.String() != "" {
		tsim.Ident = im.Name.String()
	} else {
		sl := strings.Split(im.Path.Value, "/")
		final := sl[len(sl)-1]
		if idx := strings.Index(final, ":"); idx != -1 {
			tsim.Pkg = final[idx:]
		} else {
			tsim.Pkg = final
		}
	}
	return tsim
}

var themamodpath string = filepath.Join("cue.mod", "pkg", "github.com", "grafana", "thema")

// all copied and hacked up from Thema's LoadInstancesWithThema, simply to allow setting the
// package name
func loadInstancesWithThema(modFS fs.FS, dir string, pkgname string) (*build.Instance, error) {
	var modname string
	err := fs.WalkDir(modFS, "cue.mod", func(path string, d fs.DirEntry, err error) error {
		// fs.FS implementations tend to not use path separators as expected. Use a
		// normalized one for comparisons, but retain the original for calls back into modFS.
		normpath := filepath.FromSlash(path)
		if err != nil {
			return err
		}

		if d.IsDir() {
			switch normpath {
			case filepath.Join("cue.mod", "gen"), filepath.Join("cue.mod", "usr"):
				return fs.SkipDir
			case themamodpath:
				return fmt.Errorf("path %q already exists in modFS passed to InstancesWithThema, must be absent for dynamic dependency injection", themamodpath)
			}
			return nil
		} else if normpath == filepath.Join("cue.mod", "module.cue") {
			modf, err := modFS.Open(path)
			if err != nil {
				return err
			}
			defer modf.Close() // nolint: errcheck

			b, err := io.ReadAll(modf)
			if err != nil {
				return err
			}

			modname, err = cuecontext.New().CompileBytes(b).LookupPath(cue.MakePath(cue.Str("module"))).String()
			if err != nil {
				return err
			}
			if modname == "" {
				return fmt.Errorf("InstancesWithThema requires non-empty module name in modFS' cue.mod/module.cue")
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if modname == "" {
		return nil, errors.New("cue.mod/module.cue did not exist")
	}

	modroot := filepath.FromSlash(filepath.Join("/", modname))
	overlay := make(map[string]load.Source)
	if err := tload.ToOverlay(modroot, modFS, overlay); err != nil {
		return nil, err
	}

	// Special case for when we're calling this loader with paths inside the thema module
	if modname == "github.com/grafana/thema" {
		if err := tload.ToOverlay(modroot, thema.CueJointFS, overlay); err != nil {
			return nil, err
		}
	} else {
		if err := tload.ToOverlay(filepath.Join(modroot, themamodpath), thema.CueFS, overlay); err != nil {
			return nil, err
		}
	}

	if dir == "" {
		dir = "."
	}

	cfg := &load.Config{
		Overlay:    overlay,
		ModuleRoot: modroot,
		Module:     modname,
		Dir:        filepath.Join(modroot, dir),
		Package:    pkgname,
	}
	if dir == "." {
		cfg.Package = filepath.Base(modroot)
		cfg.Dir = modroot
	}

	inst := load.Instances(nil, cfg)[0]
	if inst.Err != nil {
		return nil, inst.Err
	}

	return inst, nil
}

func toOverlay(prefix string, vfs fs.FS, overlay map[string]load.Source) error {
	if !filepath.IsAbs(prefix) {
		return fmt.Errorf("must provide absolute path prefix when generating cue overlay, got %q", prefix)
	}
	err := fs.WalkDir(vfs, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		f, err := vfs.Open(path)
		if err != nil {
			return err
		}
		defer func(f fs.File) {
			err := f.Close()
			if err != nil {
				return
			}
		}(f)

		b, err := io.ReadAll(f)
		if err != nil {
			return err
		}

		overlay[filepath.Join(prefix, path)] = load.FromBytes(b)
		return nil
	})

	if err != nil {
		return err
	}

	return nil
}

type tsFile struct {
	V                 thema.SyntacticVersion
	WriteModelVersion bool
	Imports           []*tsImport
	Body              string
}

type tsImport struct {
	Ident string
	Pkg   string
}

var tsTemplate = template.Must(template.New("cuetsygen").Parse(`//~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~
// This file is autogenerated. DO NOT EDIT.
//
// To regenerate, run "make gen-cue" from the repository root.
//~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~
{{range .Imports}}
import * as {{.Ident}} from '{{.Pkg}}';{{end}}
{{if .WriteModelVersion }}
export const modelVersion = Object.freeze([{{index .V 0}}, {{index .V 1}}]);
{{end}}
{{.Body}}`))
