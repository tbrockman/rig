package manifest

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// `make generate` runs the generated-files test with -update.
var update = flag.Bool("update", false, "rewrite schema/rig.schema.json and README.md's field table")

const (
	schemaFile  = "../../schema/rig.schema.json"
	readmeFile  = "../../README.md"
	fieldsBegin = "<!-- fields: generated from internal/manifest by `make generate` -->\n"
	fieldsEnd   = "<!-- /fields -->\n"
)

// The committed schema, and the README's table of fields, are generated from
// the types and their doc comments. Editing a field's comment without
// regenerating is a failure here, not a stale editor tooltip later.
func TestGeneratedFilesAreCurrent(t *testing.T) {
	s, err := reflectSchema(".")
	if err != nil {
		t.Fatal(err)
	}
	gen, err := GenerateSchema(".")
	if err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile(readmeFile)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(readme, []byte(fieldsBegin))
	j := bytes.Index(readme, []byte(fieldsEnd))
	if i < 0 || j < i {
		t.Fatalf("README.md has no %q ... %q section", fieldsBegin, fieldsEnd)
	}
	var want bytes.Buffer
	want.Write(readme[:i+len(fieldsBegin)])
	want.WriteString(fieldsTable(s))
	want.Write(readme[j:])

	if *update {
		if err := os.MkdirAll(filepath.Dir(schemaFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(schemaFile, gen, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(readmeFile, want.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if have, _ := os.ReadFile(schemaFile); !bytes.Equal(have, gen) {
		t.Error("schema/rig.schema.json is stale; run `make generate`")
	}
	if !bytes.Equal(readme, want.Bytes()) {
		t.Error("README.md's field table is stale; run `make generate`")
	}
}

// What a schema cannot say, and validate() does: a reference from one part of
// the file to another, two devices at one address, and numeric rules inside a
// string. Everything else validate() refuses, the schema must refuse too, so
// an editor flags it before rig does.
var schemaCannotSay = map[string]bool{
	"wanted device not declared": true,
	"duplicate pci":              true,
	"unequal ranges":             true,
	"port out of range":          true,
	"wildcard host ip":           true,
}

func TestSchemaAgreesWithValidate(t *testing.T) {
	gen, err := GenerateSchema(".")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(gen))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(SchemaURL, doc); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile(SchemaURL)
	if err != nil {
		t.Fatal(err)
	}
	check := func(text string) error {
		var v any
		if err := yaml.Unmarshal([]byte(text), &v); err != nil {
			t.Fatal(err)
		}
		// Through JSON, so the validator sees JSON's types, not YAML's.
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		return sch.Validate(inst)
	}

	good := map[string]string{"the example": example}
	for _, f := range []string{"../../examples/cuda-agent/rig.yaml", "../../project-template/rig.yaml"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.NewReplacer("PROJECT", "proj", "GPU_PCI", "0000:2b:00.0", "GPU_ID", "10de:abcd").Replace(string(b))
		good[f] = text
	}
	for name, text := range good {
		if _, err := Parse([]byte(text)); err != nil {
			t.Fatalf("%s: validate refuses it: %v", name, err)
		}
		if err := check(text); err != nil {
			t.Errorf("%s: the schema refuses what rig accepts: %v", name, err)
		}
	}

	for name, edit := range badEdits {
		err := check(edit(example))
		switch {
		case err == nil && !schemaCannotSay[name]:
			t.Errorf("%s: rig refuses it, the schema accepts it", name)
		case err != nil && schemaCannotSay[name]:
			t.Errorf("%s: the schema now refuses it; take it out of schemaCannotSay", name)
		}
	}
	if err := check(strings.Replace(example, "env_file:", "envfile:", 1)); err == nil {
		t.Error("an unknown key must be refused by the schema too")
	}
}
