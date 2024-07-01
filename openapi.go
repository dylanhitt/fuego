package fuego

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3gen"
)

func NewOpenApiSpec() openapi3.T {
	info := &openapi3.Info{
		Title:       "OpenAPI",
		Description: "OpenAPI",
		Version:     "0.0.1",
	}
	spec := openapi3.T{
		OpenAPI: "3.0.3",
		Info:    info,
		Paths:   &openapi3.Paths{},
		Components: &openapi3.Components{
			Schemas:       make(map[string]*openapi3.SchemaRef),
			RequestBodies: make(map[string]*openapi3.RequestBodyRef),
			Responses:     make(map[string]*openapi3.ResponseRef),
		},
	}
	return spec
}

// Hide prevents the routes in this server or group from being included in the OpenAPI spec.
func (s *Server) Hide() *Server {
	s.DisableOpenapi = true
	return s
}

// Show allows to display the routes. Activated by default so useless in most cases,
// but this can be useful if you desactivated the parent group.
func (s *Server) Show() *Server {
	s.DisableOpenapi = false
	return s
}

// OutputOpenAPISpec takes the OpenAPI spec and outputs it to a JSON file and/or serves it on a URL.
// Also serves a Swagger UI.
// To modify its behavior, use the [WithOpenAPIConfig] option.
func (s *Server) OutputOpenAPISpec() openapi3.T {
	// Validate
	err := s.OpenApiSpec.Validate(context.Background())
	if err != nil {
		slog.Error("Error validating spec", "error", err)
	}

	// Marshal spec to JSON
	jsonSpec, err := s.marshalSpec()
	if err != nil {
		slog.Error("Error marshalling spec to JSON", "error", err)
	}

	if !s.OpenAPIConfig.DisableSwagger {
		s.registerOpenAPIRoutes(jsonSpec)
	}

	if !s.OpenAPIConfig.DisableLocalSave {
		err := s.saveOpenAPIToFile(s.OpenAPIConfig.JsonFilePath, jsonSpec)
		if err != nil {
			slog.Error("Error saving spec to local path", "error", err, "path", s.OpenAPIConfig.JsonFilePath)
		}
	}

	return s.OpenApiSpec
}

func (s *Server) marshalSpec() ([]byte, error) {
	if s.OpenAPIConfig.PrettyFormatJson {
		return json.MarshalIndent(s.OpenApiSpec, "", "	")
	}
	return json.Marshal(s.OpenApiSpec)
}

func (s *Server) saveOpenAPIToFile(jsonSpecLocalPath string, jsonSpec []byte) error {
	jsonFolder := filepath.Dir(jsonSpecLocalPath)

	err := os.MkdirAll(jsonFolder, 0o750)
	if err != nil {
		return errors.New("error creating docs directory")
	}

	f, err := os.Create(jsonSpecLocalPath) // #nosec G304 (file path provided by developer, not by user)
	if err != nil {
		return errors.New("error creating file")
	}
	defer f.Close()

	_, err = f.Write(jsonSpec)
	if err != nil {
		return errors.New("error writing file ")
	}

	s.printOpenAPIMessage("JSON file: " + jsonSpecLocalPath)
	return nil
}

// Registers the routes to serve the OpenAPI spec and Swagger UI.
func (s *Server) registerOpenAPIRoutes(jsonSpec []byte) {
	GetStd(s, s.OpenAPIConfig.JsonUrl, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jsonSpec)
	})

	Register(s, Route[any, any]{
		Method: http.MethodGet,
		Path:   s.OpenAPIConfig.SwaggerUrl + "/",
	}, s.OpenAPIConfig.UIHandler(s.OpenAPIConfig.JsonUrl))

	s.printOpenAPIMessage(fmt.Sprintf("JSON spec: %s://%s%s", s.proto(), s.Server.Addr, s.OpenAPIConfig.JsonUrl))
	s.printOpenAPIMessage(fmt.Sprintf("OpenAPI UI: %s://%s%s/index.html", s.proto(), s.Server.Addr, s.OpenAPIConfig.SwaggerUrl))
}

func (s *Server) printOpenAPIMessage(msg string) {
	if !s.disableStartupMessages {
		slog.Info(msg)
	}
}

func validateJsonSpecLocalPath(jsonSpecLocalPath string) bool {
	jsonSpecLocalPathRegexp := regexp.MustCompile(`^[^\/][\/a-zA-Z0-9\-\_]+(.json)$`)
	return jsonSpecLocalPathRegexp.MatchString(jsonSpecLocalPath)
}

func validateJsonSpecUrl(jsonSpecUrl string) bool {
	jsonSpecUrlRegexp := regexp.MustCompile(`^\/[\/a-zA-Z0-9\-\_]+(.json)$`)
	return jsonSpecUrlRegexp.MatchString(jsonSpecUrl)
}

func validateSwaggerUrl(swaggerUrl string) bool {
	swaggerUrlRegexp := regexp.MustCompile(`^\/[\/a-zA-Z0-9\-\_]+[a-zA-Z0-9\-\_]$`)
	return swaggerUrlRegexp.MatchString(swaggerUrl)
}

var generator = openapi3gen.NewGenerator(
	openapi3gen.UseAllExportedFields(),
)

// RegisterOpenAPIOperation registers an OpenAPI operation.
func RegisterOpenAPIOperation[T, B any](s *Server, method, path string) (*openapi3.Operation, error) {
	operation := openapi3.NewOperation()

	operation.Tags = s.tags

	// Tags
	if !s.disableAutoGroupTags && s.groupTag != "" {
		operation.Tags = append(operation.Tags, s.groupTag)
	}

	// Request body
	bodyTag := schemaTagFromType[B](s, *new(B))
	if (method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch) && bodyTag.name != "unknown-interface" && bodyTag.name != "string" {
		content := openapi3.NewContentWithSchemaRef(&bodyTag.SchemaRef, []string{"application/json"})
		requestBody := openapi3.NewRequestBody().
			WithRequired(true).
			WithDescription("Request body for " + reflect.TypeOf(*new(B)).String()).
			WithContent(content)

		s.OpenApiSpec.Components.RequestBodies[bodyTag.name] = &openapi3.RequestBodyRef{
			Value: requestBody,
		}

		// add request body to operation
		operation.RequestBody = &openapi3.RequestBodyRef{
			Ref:   "#/components/requestBodies/" + bodyTag.name,
			Value: requestBody,
		}
	}

	responseSchema := schemaTagFromType[T](s, *new(T))
	content := openapi3.NewContentWithSchemaRef(&responseSchema.SchemaRef, []string{"application/json"})
	response := openapi3.NewResponse().
		WithDescription("OK").
		WithContent(content)

	operation.AddResponse(200, response)

	// Path parameters
	for _, pathParam := range parsePathParams(path) {
		parameter := openapi3.NewPathParameter(pathParam)
		parameter.Schema = openapi3.NewStringSchema().NewRef()
		operation.AddParameter(parameter)
	}

	s.OpenApiSpec.AddOperation(path, method, operation)

	return operation, nil
}

type schemaTag struct {
	openapi3.SchemaRef
	name string
}

func schemaTagFromType[V any](s *Server, v any) schemaTag {
	if v == nil {
		// ensure we add unknown-interface to our schemas
		s.getOrCreateSchema("unknown-interface", struct{}{})
		return schemaTag{
			name: "unknown-interface",
			SchemaRef: openapi3.SchemaRef{
				Ref: "#/components/schemas/unknown-interface",
			},
		}
	}

	return dive[V](s, reflect.TypeOf(v), schemaTag{}, 5)
}

// dive returns a schemaTag which includes the generated openapi3.SchemaRef and
// the name of the struct being passed in.
// If the type is a pointer, map, channel, function, or unsafe pointer,
// it will dive into the type and return the name of the type it points to.
// If the type is a slice or array type it will dive into the type as well as
// build and openapi3.Schema where Type is array and Ref is set to the proper
// components Schema
func dive[V any](s *Server, t reflect.Type, tag schemaTag, maxDepth int) schemaTag {
	if maxDepth == 0 {
		return schemaTag{
			name: "default",
			SchemaRef: openapi3.SchemaRef{
				Ref: "#/components/schemas/default",
			},
		}
	}

	switch t.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return dive[V](s, t.Elem(), tag, maxDepth-1)

	case reflect.Slice, reflect.Array:
		item := dive[V](s, t.Elem(), tag, maxDepth-1)
		tag.name = item.name
		tag.Value = &openapi3.Schema{
			Type:  "array",
			Items: &item.SchemaRef,
		}
		return tag

	default:
		tag.name = t.Name()
		tag.Ref = "#/components/schemas/" + tag.name
		tag.Value = s.getOrCreateSchema(tag.name, new(V))
		return tag
	}
}

func (s *Server) getOrCreateSchema(key string, v any) *openapi3.Schema {
	schemaRef, ok := s.OpenApiSpec.Components.Schemas[key]
	if !ok {
		schemaRef, _ = generator.NewSchemaRefForValue(v, s.OpenApiSpec.Components.Schemas)
		s.OpenApiSpec.Components.Schemas[key] = schemaRef
	}
	return schemaRef.Value
}
