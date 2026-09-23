package manifest

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/invopop/jsonschema"
)

// SchemaURL is where the committed schema is published, for editors to fetch.
// `rig init` points a new rig.yaml at the copy matching the rig that wrote it.
const SchemaURL = "https://raw.githubusercontent.com/tbrockman/rig/main/schema/rig.schema.json"

// modulePath is this package's import path, which the reflector keys doc
// comments by.
const modulePath = "github.com/tbrockman/rig/internal/manifest"

// GenerateSchema reflects the JSON Schema for rig.yaml from the types in this
// package. With the package's source directory, their doc comments become the
// descriptions an editor shows; without it the schema has none. The result is
// committed as schema/rig.schema.json, so the binary never needs the source.
//
// The schema covers the file's shape: keys, types, enums, patterns, and the
// rules that depend on one field's value. validate() still has the last word,
// for what a schema cannot say (a device listed in guest.devices that
// host.devices does not declare, two devices at one address) and for error
// messages that name the fix.
func GenerateSchema(sourceDir string) ([]byte, error) {
	s, err := reflectSchema(sourceDir)
	if err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func reflectSchema(sourceDir string) (*jsonschema.Schema, error) {
	r := &jsonschema.Reflector{
		FieldNameTag:               "yaml",
		RequiredFromJSONSchemaTags: true,
		ExpandedStruct:             true,
	}
	if sourceDir != "" {
		if err := r.AddGoComments(modulePath, sourceDir); err != nil {
			return nil, err
		}
		// Comments are wrapped for Go; an editor wraps descriptions itself.
		for k, v := range r.CommentMap {
			r.CommentMap[k] = strings.Join(strings.Fields(v), " ")
		}
	}
	s := r.Reflect(&Manifest{})
	s.ID = jsonschema.ID(SchemaURL)
	s.Title = "rig.yaml"
	return s, nil
}

// fieldsTable renders the fields and their descriptions as the Markdown table
// in README.md, so the README says what the schema says.
func fieldsTable(s *jsonschema.Schema) string {
	var b strings.Builder
	b.WriteString("| Field | |\n|---|---|\n")
	// Markdown would take a | for a cell border and <name> for an HTML tag.
	cell := strings.NewReplacer("|", "\\|", "<", "&lt;", ">", "&gt;").Replace
	rows := func(prefix, def string) {
		for name, p := range s.Definitions[def].Properties.FromOldest() {
			fmt.Fprintf(&b, "| `%s%s` | %s |\n", prefix, name, cell(p.Description))
			if name == "return" {
				for sub, q := range s.Definitions["Return"].Properties.FromOldest() {
					fmt.Fprintf(&b, "| `%sreturn.%s` | %s |\n", prefix, sub, cell(q.Description))
				}
			}
		}
	}
	rows("host.devices.<name>.", "Device")
	rows("guest.", "Guest")
	return b.String()
}

// The pieces below are what reflection cannot see: patterns shared with
// validate(), fields with a short and a long form, and rules that depend on
// another field's value. Each reuses the regexp validate() uses, so the two
// cannot disagree about a format.

func prop(s *jsonschema.Schema, name string) *jsonschema.Schema {
	p, _ := s.Properties.Get(name)
	return p
}

func required(names ...string) *jsonschema.Schema {
	return &jsonschema.Schema{Required: names}
}

// whenConst applies then to an object whose field is value.
func whenConst(field, value string, then *jsonschema.Schema) *jsonschema.Schema {
	props := jsonschema.NewProperties()
	props.Set(field, &jsonschema.Schema{Const: value})
	return &jsonschema.Schema{
		If:   &jsonschema.Schema{Properties: props, Required: []string{field}},
		Then: then,
	}
}

func (Host) JSONSchemaExtend(s *jsonschema.Schema) {
	p := prop(s, "devices")
	p.PropertyNames = &jsonschema.Schema{Pattern: nameRE.String()}
	// A devices: whose entries are all commented out, as `rig init` writes
	// it, is YAML's null; rig reads that as no devices.
	obj := *p
	*p = jsonschema.Schema{Description: obj.Description, AnyOf: []*jsonschema.Schema{{Type: "null"}, &obj}}
}

func (Device) JSONSchemaExtend(s *jsonschema.Schema) {
	prop(s, "pci").Pattern = pciRE.String()
	prop(s, "id").Pattern = usbRE.String()
	s.AllOf = []*jsonschema.Schema{
		whenConst("kind", KindGPU, required("pci")),
		whenConst("kind", KindPCI, required("pci", "id")),
		whenConst("kind", KindUSB, &jsonschema.Schema{
			Required: []string{"id"},
			// The host keeps a USB device's controller: nothing to address
			// and nothing to return.
			Not: &jsonschema.Schema{AnyOf: []*jsonschema.Schema{required("pci"), required("return")}},
		}),
	}
}

func (Return) JSONSchemaExtend(s *jsonschema.Schema) {
	prop(s, "modules").Items.Pattern = `^[a-zA-Z0-9_-]+$`
	// A glob under the device's own sysfs directory, never a path out of it.
	prop(s, "alive").Not = &jsonschema.Schema{AnyOf: []*jsonschema.Schema{
		{Pattern: `^/`}, {Pattern: `\.\.`},
	}}
}

func (Guest) JSONSchemaExtend(s *jsonschema.Schema) {
	prop(s, "name").Pattern = nameRE.String()
	prop(s, "devices").Items.Pattern = nameRE.String()
	s.AnyOf = []*jsonschema.Schema{required("image"), required("flake")}
	// A published port needs a network device.
	s.AllOf = []*jsonschema.Schema{
		whenConst("network", NetworkNone, &jsonschema.Schema{Not: required("ports")}),
	}
}

func (CPUs) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{OneOf: []*jsonschema.Schema{
		{Type: "integer", Minimum: "1"},
		{Type: "string", Pattern: cpuSetRE.String()},
	}}
}

// shortVolumeRE is Compose's "name:/path", with a named volume on the left.
var shortVolumeRE = `^[a-z0-9][a-z0-9_-]{0,62}:/.+$`

func (Volume) JSONSchemaExtend(s *jsonschema.Schema) {
	prop(s, "source").Pattern = volumeNameRE.String()
	prop(s, "target").Pattern = `^/.+`
	prop(s, "owner").Pattern = ownerRE.String()
	long := *s
	*s = jsonschema.Schema{
		Description: long.Description,
		OneOf:       []*jsonschema.Schema{{Type: "string", Pattern: shortVolumeRE}, &long},
	}
}

// portSpecRE is one port or an inclusive range, as a string.
const portSpecRE = `^[0-9]+(-[0-9]+)?$`

func (Port) JSONSchemaExtend(s *jsonschema.Schema) {
	spec := func() *jsonschema.Schema {
		return &jsonschema.Schema{OneOf: []*jsonschema.Schema{
			{Type: "integer", Minimum: "1", Maximum: "65535"},
			{Type: "string", Pattern: portSpecRE},
		}}
	}
	for _, name := range []string{"published", "target"} {
		p := prop(s, name)
		desc := p.Description
		*p = *spec()
		p.Description = desc
	}
	prop(s, "host_ip").Pattern = `^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`
	long := *s
	long.AnyOf = []*jsonschema.Schema{required("published"), required("target")}
	*s = jsonschema.Schema{
		Description: long.Description,
		OneOf:       []*jsonschema.Schema{{Type: "string", Pattern: portRE.String()}, &long},
	}
}
