package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/types"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/gojuno/generator"
	"github.com/pkg/errors"
	"golang.org/x/tools/go/loader"
)

var (
	interfaceName = flag.String("i", "", "interface name")
	structName    = flag.String("s", "", "target struct name, default: <interface name>Tracer")
	outputFile    = flag.String("o", "", "output filename")
)

const template = `
	type {{$structName}} struct {
		next {{$interfaceName}}
		prefix string
	}

	func New{{$structName}}(next {{$interfaceName}}, prefix string) *{{$structName}} {
		return &{{$structName}} {
			next: next,
			prefix: prefix,
		}
	}

	{{ range $methodName, $method := . }}
		func (t *{{$structName}}) {{$methodName}}{{signature $method}} {
			span, ctx := opentracing.StartSpanFromContext(ctx, t.prefix + ".{{$interfaceName}}.{{$methodName}}")
			defer span.Finish()

			return t.next.{{$methodName}}({{call $method}})
		}
	{{ end }}
	`

func main() {
	flag.Parse()

	if *interfaceName == "" || *outputFile == "" || flag.NArg() != 1 {
		flag.Usage()
		os.Exit(1)
	}

	if *structName == "" {
		*structName = fmt.Sprintf("%vTracer", *interfaceName)
	}

	fn := func(s *types.Signature) error {
		if s.Params().Len() == 0 || s.Params().At(0).Type().String() != "context.Context" {
			return errors.Errorf("first param must be context.Context")
		}

		return nil
	}

	gen := NewDecoratorGenerator(template, []validatorFunc{fn})
	err := gen.Generate(flag.Arg(0), *interfaceName, *outputFile, *structName)
	if err != nil {
		log.Fatal(err)
	}
}

type validatorFunc func(s *types.Signature) error

type DecoratorGenerator struct {
	template   string
	validators []validatorFunc
}

func NewDecoratorGenerator(template string, validators []validatorFunc) *DecoratorGenerator {
	return &DecoratorGenerator{
		template:   template,
		validators: validators,
	}
}

func (g *DecoratorGenerator) Generate(sourcePackage, interfaceName, outputFile, outputStruct string) error {
	sourcePath, err := generator.PackageOf(sourcePackage)
	if err != nil {
		return errors.Wrap(err, "failed to get sourcePath")
	}

	destPath, err := generator.PackageOf(filepath.Dir(outputFile))
	if err != nil {
		return errors.Wrap(err, "failed to get destPath")
	}

	program, err := g.createProgram(sourcePath, destPath)
	if err != nil {
		return errors.Wrap(err, "failed to create program")
	}

	gen := generator.New(program)

	_, sourcePackageName := gen.PackagePathAndName(sourcePath)
	_, destPackageName := gen.PackagePathAndName(destPath)

	gen.SetPackageName(destPackageName)
	gen.SetVar("structName", outputStruct)
	gen.AddTemplateFunc("call", FuncCall(gen))
	gen.ImportWithAlias(destPath, "")

	if sourcePath != destPath {
		gen.SetVar("interfaceName", fmt.Sprintf("%v.%v", sourcePackageName, interfaceName))
	} else {
		gen.SetVar("interfaceName", interfaceName)
	}

	v := &visitor{
		gen:        gen,
		methods:    make(map[string]*types.Signature),
		sname:      interfaceName,
		validators: g.validators,
	}
	for _, file := range program.Package(sourcePath).Files {
		ast.Walk(v, file)
	}

	if v.err != nil {
		return errors.Wrap(v.err, "failed to parse interface")
	}

	if err := gen.ProcessTemplate("interface", template, v.methods); err != nil {
		return errors.Wrap(err, "failed to process template")
	}

	if err := gen.WriteToFilename(outputFile); err != nil {
		return errors.Wrap(err, "failed to write file")
	}

	return nil
}

func (g *DecoratorGenerator) createProgram(sourcePath, destPath string) (*loader.Program, error) {
	config := loader.Config{}

	config.Import(sourcePath)
	config.Import(destPath)

	return config.Load()
}

// visitor collects all methods of specified interface
type visitor struct {
	gen        *generator.Generator
	methods    map[string]*types.Signature
	sname      string
	validators []validatorFunc
	err        error
}

// Visit is implementation of ast.Visitor interface
func (v *visitor) Visit(node ast.Node) (w ast.Visitor) {
	if ts, ok := node.(*ast.TypeSpec); ok {
		exprType, err := v.gen.ExpressionType(ts.Type)
		if err != nil {
			log.Fatal(err)
		}

		switch t := exprType.(type) {
		case *types.Interface:
			if ts.Name.Name != v.sname {
				return v
			}

			if v.err == nil {
				v.err = v.processInterface(t)
			}
		}
	}

	return v
}

func (v *visitor) processInterface(t *types.Interface) error {
	for i := 0; i < t.NumMethods(); i++ {
		name := t.Method(i).Name()
		signature := t.Method(i).Type().(*types.Signature)
		for _, validator := range v.validators {
			err := validator(signature)
			if err != nil {
				return errors.Wrapf(err, "failed to validate method '%v'", name)
			}
		}
		v.methods[name] = signature
	}

	return nil
}

//FuncCall returns a signature of the function represented by f
//f can be one of: ast.Expr, ast.SelectorExpr, types.Type, types.Signature
func FuncCall(g *generator.Generator) interface{} {
	return func(f interface{}) (string, error) {
		params, err := g.FuncParams(f)
		if err != nil {
			return "", fmt.Errorf("failed to get %+v func params: %v", f, err)
		}

		names := []string{}
		for _, param := range params {
			names = append(names, param.Pass())
		}

		return strings.Join(names, ", "), nil
	}
}
