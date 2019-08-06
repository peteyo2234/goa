package codegen

import (
	"fmt"
	"path/filepath"
	"strings"

	"goa.design/goa/v3/codegen"
	"goa.design/goa/v3/expr"
)

// ServerTypeFiles returns the HTTP transport type files.
func ServerTypeFiles(genpkg string, root *expr.RootExpr) []*codegen.File {
	fw := make([]*codegen.File, len(root.API.HTTP.Services))
	seen := make(map[string]struct{})
	for i, r := range root.API.HTTP.Services {
		fw[i] = serverType(genpkg, r, seen)
	}
	return fw
}

// serverType return the file containing the type definitions used by the HTTP
// transport for the given service server. seen keeps track of the names of the
// types that have already been generated to prevent duplicate code generation.
//
// Below are the rules governing whether values are pointers or not. Note that
// the rules only applies to values that hold primitive types, values that hold
// slices, maps or objects always use pointers either implicitly - slices and
// maps - or explicitly - objects.
//
//   * The payload struct fields (if a struct) hold pointers when not required
//     and have no default value.
//
//   * Request body fields (if the body is a struct) always hold pointers to
//     allow for explicit validation.
//
//   * Request header, path and query string parameter variables hold pointers
//     when not required. Request header, body fields and param variables that
//     have default values are never required (enforced by DSL engine).
//
//   * The result struct fields (if a struct) hold pointers when not required
//     or have a default value (so generated code can set when null)
//
//   * Response body fields (if the body is a struct) and header variables hold
//     pointers when not required and have no default value.
//
func serverType(genpkg string, svc *expr.HTTPServiceExpr, seen map[string]struct{}) *codegen.File {
	var (
		path    string
		data    = HTTPServices.Get(svc.Name())
		svcName = codegen.SnakeCase(data.Service.VarName)
	)
	path = filepath.Join(codegen.Gendir, "http", svcName, "server", "types.go")
	header := codegen.Header(svc.Name()+" HTTP server types", "server",
		[]*codegen.ImportSpec{
			{Path: "unicode/utf8"},
			{Path: genpkg + "/" + svcName, Name: data.Service.PkgName},
			codegen.GoaImport(""),
			{Path: genpkg + "/" + svcName + "/" + "views", Name: data.Service.ViewsPkg},
		},
	)

	var (
		initData       []*InitData
		validatedTypes []*TypeData

		sections = []*codegen.SectionTemplate{header}
	)

	// request body types
	for _, a := range svc.HTTPEndpoints {
		adata := data.Endpoint(a.Name())
		if data := adata.Payload.Request.ServerBody; data != nil {
			if data.Def != "" {
				sections = append(sections, &codegen.SectionTemplate{
					Name:   "request-body-type-decl",
					Source: typeDeclT,
					Data:   data,
				})
			}
			if data.ValidateDef != "" {
				validatedTypes = append(validatedTypes, data)
			}
		}
		if adata.ServerStream != nil {
			if data := adata.ServerStream.Payload; data != nil {
				if data.Def != "" {
					sections = append(sections, &codegen.SectionTemplate{
						Name:   "request-body-type-decl",
						Source: typeDeclT,
						Data:   data,
					})
				}
				if data.ValidateDef != "" {
					validatedTypes = append(validatedTypes, data)
				}
			}
		}
	}

	// response body types
	for _, a := range svc.HTTPEndpoints {
		adata := data.Endpoint(a.Name())
		for _, resp := range adata.Result.Responses {
			for _, tdata := range resp.ServerBody {
				if generated, ok := data.ServerTypeNames[tdata.Name]; ok && !generated {
					if tdata.Def != "" {
						sections = append(sections, &codegen.SectionTemplate{
							Name:   "response-server-body",
							Source: typeDeclT,
							Data:   tdata,
						})
					}
					if tdata.Init != nil {
						initData = append(initData, tdata.Init)
					}
					if tdata.ValidateDef != "" {
						validatedTypes = append(validatedTypes, tdata)
					}
					data.ServerTypeNames[tdata.Name] = true
				}
			}
		}
	}

	// error body types
	for _, a := range svc.HTTPEndpoints {
		adata := data.Endpoint(a.Name())
		for _, gerr := range adata.Errors {
			for _, herr := range gerr.Errors {
				for _, data := range herr.Response.ServerBody {
					if data.Def != "" {
						sections = append(sections, &codegen.SectionTemplate{
							Name:   "error-body-type-decl",
							Source: typeDeclT,
							Data:   data,
						})
					}
					if data.Init != nil {
						initData = append(initData, data.Init)
					}
					if data.ValidateDef != "" {
						validatedTypes = append(validatedTypes, data)
					}
				}
			}
		}
	}

	// body attribute types
	for _, tdata := range data.ServerBodyAttributeTypes {
		if tdata.Def != "" {
			sections = append(sections, &codegen.SectionTemplate{
				Name:   "server-body-attributes",
				Source: typeDeclT,
				Data:   tdata,
			})
		}

		if tdata.ValidateDef != "" {
			validatedTypes = append(validatedTypes, tdata)
		}
	}

	// body constructors
	for _, init := range initData {
		sections = append(sections, &codegen.SectionTemplate{
			Name:   "server-body-init",
			Source: serverBodyInitT,
			Data:   init,
		})
	}

	fm := map[string]interface{}{
		"fieldCode": fieldCode,
	}
	for _, adata := range data.Endpoints {
		// request to method payload
		if init := adata.Payload.Request.PayloadInit; init != nil {
			sections = append(sections, &codegen.SectionTemplate{
				Name:    "server-payload-init",
				Source:  serverTypeInitT,
				Data:    init,
				FuncMap: fm,
			})
		}
		if adata.ServerStream != nil && adata.ServerStream.Payload != nil {
			if init := adata.ServerStream.Payload.Init; init != nil {
				sections = append(sections, &codegen.SectionTemplate{
					Name:    "server-payload-init",
					Source:  serverTypeInitT,
					Data:    init,
					FuncMap: fm,
				})
			}
		}
	}

	// validate methods
	for _, data := range validatedTypes {
		sections = append(sections, &codegen.SectionTemplate{
			Name:   "server-validate",
			Source: validateT,
			Data:   data,
		})
	}

	return &codegen.File{Path: path, SectionTemplates: sections}
}

// fieldCode generates code to initialize the data structures fields
// from the given args. It is used only in templates.
func fieldCode(args []*InitArgData, tgtVar, tgtPkg string) string {
	var code string
	scope := codegen.NewNameScope()
	for _, arg := range args {
		if arg.FieldName == "" {
			continue
		}
		_, isut := arg.FieldType.(expr.UserType)
		switch {
		case expr.Equal(arg.Type, arg.FieldType):
			// no need to call transform for same type
			deref := ""
			if !arg.Pointer && arg.FieldPointer && expr.IsPrimitive(arg.FieldType) {
				deref = "&"
			}
			code += fmt.Sprintf("%s.%s = %s%s\n", tgtVar, arg.FieldName, deref, arg.Name)
		case expr.IsPrimitive(arg.FieldType) && isut:
			t := scope.GoFullTypeRef(&expr.AttributeExpr{Type: arg.FieldType}, tgtPkg)
			cast := fmt.Sprintf("%s(%s)", t, arg.Name)
			if arg.Pointer {
				cast = fmt.Sprintf("%s(*%s)", t, arg.Name)
			}
			if arg.FieldPointer {
				code += fmt.Sprintf("tmp%s := %s\n%s.%s = &tmp%s\n", arg.Name, cast, tgtVar, arg.FieldName, arg.Name)
			} else {
				code += fmt.Sprintf("%s.%s = %s\n", tgtVar, arg.FieldName, cast)
			}
		default:
			srcctx := codegen.NewAttributeContext(arg.Pointer, false, true, "", scope)
			tgtctx := codegen.NewAttributeContext(arg.FieldPointer, false, true, tgtPkg, scope)
			c, h, err := codegen.GoTransformToVar(
				&expr.AttributeExpr{Type: arg.Type}, &expr.AttributeExpr{Type: arg.FieldType},
				arg.Name, fmt.Sprintf("%s.%s", tgtVar, arg.FieldName), srcctx, tgtctx, "", false)
			if err != nil {
				panic(err) // bug
			}
			if len(h) > 0 {
				panic("expected 0 transform helpers but got at least 1") // bug
			}
			code += c + "\n"
		}
	}
	return strings.Trim(code, "\n")
}

// input: TypeData
const typeDeclT = `{{ comment .Description }}
type {{ .VarName }} {{ .Def }}
`

// input: InitData
const serverTypeInitT = `{{ comment .Description }}
func {{ .Name }}({{- range .ServerArgs }}{{ .Name }} {{ .TypeRef }}, {{ end }}) {{ .ReturnTypeRef }} {
{{- if .ServerCode }}
	{{ .ServerCode }}
{{- else if .ReturnIsStruct }}
	v := &{{ .ReturnTypeName }}{}
{{- end }}
{{- if .ReturnTypeAttribute }}
	res := &{{ .ReturnTypeName }}{
		{{ .ReturnTypeAttribute }}: v,
	}
{{- end }}
{{- if .ReturnIsStruct }}
	{{- if .ReturnTypeAttribute }}
		{{- $code := (fieldCode .ServerArgs "res" .ReturnTypePkg) }}
		{{- if $code }}
			{{ $code }}
		{{- end }}
	{{- else }}
		{{- $code := (fieldCode .ServerArgs "v" .ReturnTypePkg) }}
		{{- if $code }}
			{{ $code }}
		{{- end }}
	{{- end }}
{{- end }}
	return {{ if .ReturnTypeAttribute }}res{{ else }}v{{ end }}
}
`

// input: InitData
const serverBodyInitT = `{{ comment .Description }}
func {{ .Name }}({{ range .ServerArgs }}{{ .Name }} {{.TypeRef }}, {{ end }}) {{ .ReturnTypeRef }} {
	{{ .ServerCode }}
	return body
}
`

// input: TypeData
const validateT = `{{ printf "Validate%s runs the validations defined on %s" .VarName .Name | comment }}
func Validate{{ .VarName }}(body {{ .Ref }}) (err error) {
	{{ .ValidateDef }}
	return
}
`
