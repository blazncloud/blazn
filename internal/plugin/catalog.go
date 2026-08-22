package plugin

import (
	"errors"
	"sort"
)

const ProtocolVersion = 1

type Definition struct {
	Name               string   `json:"name"`
	Repository         string   `json:"repository"`
	Executable         string   `json:"executable"`
	CanonicalCommand   string   `json:"canonicalCommand"`
	Aliases            []string `json:"aliases"`
	SigningIdentity    string   `json:"signingIdentity"`
	SignatureNamespace string   `json:"signatureNamespace"`
	AllowedSigner      string   `json:"-"`
}

type Catalog struct {
	plugins  map[string]Definition
	commands map[string]string
}

var socialDefinition = Definition{
	Name:               "social",
	Repository:         "blazncloud/blazn-social",
	Executable:         "blazn-social",
	CanonicalCommand:   "social",
	Aliases:            []string{"person", "company", "contact", "connections", "post", "evidence", "entity", "data", "providers"},
	SigningIdentity:    "blazn-social-release",
	SignatureNamespace: "blazn-social-release",
	AllowedSigner:      `blazn-social-release namespaces="blazn-social-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAID5dgrZCi276ezBnP1qZBMvwK8bRBAzkXhC5nk/VC7uT blazn-social-release-v1`,
}

var contentDefinition = Definition{
	Name:               "content",
	Repository:         "blazncloud/blazn-content",
	Executable:         "blazn-content",
	CanonicalCommand:   "content",
	Aliases:            []string{"media", "image", "video", "audio", "render", "remix"},
	SigningIdentity:    "blazn-content-release",
	SignatureNamespace: "blazn-content-release",
	AllowedSigner:      `blazn-content-release namespaces="blazn-content-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOGkctg037D7qw5pChuiv48yUl6Y8wiTJ3Q+cgUxvq5c blazn-content-release-v1`,
}

func DefaultCatalog() Catalog {
	catalog, err := NewCatalog([]Definition{socialDefinition, contentDefinition}, []string{"auth", "doctor", "help", "node", "plugins", "uninstall", "version", "workspace"})
	if err != nil {
		panic(err)
	}
	return catalog
}

func NewCatalog(definitions []Definition, reserved []string) (Catalog, error) {
	plugins := make(map[string]Definition, len(definitions))
	commands := make(map[string]string)
	for _, command := range reserved {
		commands[command] = "@core"
	}
	for _, definition := range definitions {
		if definition.Name == "" || definition.Repository == "" || definition.Executable == "" || definition.CanonicalCommand == "" {
			return Catalog{}, errors.New("plugin catalog entry is incomplete")
		}
		if _, exists := plugins[definition.Name]; exists {
			return Catalog{}, errors.New("plugin catalog contains a duplicate plugin")
		}
		owned := append([]string{definition.CanonicalCommand}, definition.Aliases...)
		for _, command := range owned {
			if command == "" {
				return Catalog{}, errors.New("plugin catalog contains an empty command")
			}
			if _, exists := commands[command]; exists {
				return Catalog{}, errors.New("plugin catalog contains conflicting command ownership")
			}
			commands[command] = definition.Name
		}
		plugins[definition.Name] = definition
	}
	return Catalog{plugins: plugins, commands: commands}, nil
}

func (c Catalog) Resolve(command string) (Definition, bool) {
	name, ok := c.commands[command]
	if !ok || name == "@core" {
		return Definition{}, false
	}
	definition, ok := c.plugins[name]
	return definition, ok
}

func (c Catalog) Plugin(name string) (Definition, bool) {
	definition, ok := c.plugins[name]
	return definition, ok
}

func (c Catalog) Plugins() []Definition {
	result := make([]Definition, 0, len(c.plugins))
	for _, definition := range c.plugins {
		result = append(result, definition)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}
