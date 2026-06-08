package openapi

// The Swagger 2.0 document model. Only the fields dbrest emits are present; an
// empty field is omitted so the document stays close to PostgREST's output.
// Object-valued maps (paths, definitions, parameters) marshal with sorted keys,
// which makes the document deterministic without a custom ordering.

type document struct {
	Swagger             string                     `json:"swagger"`
	Info                info                       `json:"info"`
	Host                string                     `json:"host,omitempty"`
	BasePath            string                     `json:"basePath,omitempty"`
	Schemes             []string                   `json:"schemes,omitempty"`
	Consumes            []string                   `json:"consumes,omitempty"`
	Produces            []string                   `json:"produces,omitempty"`
	Paths               map[string]*pathItem       `json:"paths"`
	Definitions         map[string]*schemaObject   `json:"definitions"`
	Parameters          map[string]*parameter      `json:"parameters,omitempty"`
	SecurityDefinitions map[string]*securityScheme `json:"securityDefinitions,omitempty"`
}

type info struct {
	Title   string `json:"title"`
	Version string `json:"version"`
}

type pathItem struct {
	Get    *operation `json:"get,omitempty"`
	Post   *operation `json:"post,omitempty"`
	Patch  *operation `json:"patch,omitempty"`
	Delete *operation `json:"delete,omitempty"`
}

type operation struct {
	Tags       []string              `json:"tags,omitempty"`
	Summary    string                `json:"summary,omitempty"`
	Parameters []*parameter          `json:"parameters,omitempty"`
	Responses  map[string]*response  `json:"responses"`
	Security   []map[string][]string `json:"security,omitempty"`
}

type parameter struct {
	Ref         string        `json:"$ref,omitempty"`
	Name        string        `json:"name,omitempty"`
	In          string        `json:"in,omitempty"`
	Description string        `json:"description,omitempty"`
	Required    bool          `json:"required,omitempty"`
	Type        string        `json:"type,omitempty"`
	Format      string        `json:"format,omitempty"`
	Schema      *schemaObject `json:"schema,omitempty"`
}

type response struct {
	Description string `json:"description"`
}

type schemaObject struct {
	Ref        string                     `json:"$ref,omitempty"`
	Type       string                     `json:"type,omitempty"`
	Required   []string                   `json:"required,omitempty"`
	Properties map[string]*propertySchema `json:"properties,omitempty"`
}

type propertySchema struct {
	Type        string `json:"type,omitempty"`
	Format      string `json:"format,omitempty"`
	Description string `json:"description,omitempty"`
}

type securityScheme struct {
	Type string `json:"type"`
	Name string `json:"name"`
	In   string `json:"in"`
}
