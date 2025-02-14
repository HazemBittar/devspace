package patch

import (
	"fmt"

	"github.com/vmware-labs/yaml-jsonpath/pkg/yamlpath"
	yaml "gopkg.in/yaml.v3"
)

// Op is a type alias
type Op string

// Ops
const (
	opAdd     Op = "add"
	opRemove  Op = "remove"
	opReplace Op = "replace"
)

type Operation struct {
	Op    Op         `yaml:"op,omitempty"`
	Path  OpPath     `yaml:"path,omitempty"`
	Value *yaml.Node `yaml:"value,omitempty"`
}

// Perform executes the operation on the given container
func (op *Operation) Perform(doc *yaml.Node) error {
	path, err := yamlpath.NewPath(string(op.Path))
	if err != nil {
		return err
	}

	matches, err := path.Find(doc)
	if err != nil {
		return err
	}

	if len(matches) == 0 {
		if op.Op == opAdd {
			matches, err = getParents(doc, op.Path)
			if err != nil {
				return fmt.Errorf("could not add using path: %s", op.Path)
			}

			parentPath := op.Path.getParentPath()
			propertName := op.Path.getChildName()
			propertyValue := op.Value.Content[0]
			op.Value = createMappingNode(propertName, propertyValue)
			op.Path = OpPath(parentPath)
		} else {
			return fmt.Errorf("%s operation does not apply: doc is missing path: %s", op.Op, op.Path)
		}
	}

	for _, match := range matches {
		parent := find(doc, containsChild(match))

		switch op.Op {
		case opAdd:
			op.add(parent, match)
		case opRemove:
			op.remove(parent, match)
		case opReplace:
			op.replace(parent, match)
		default:
			return fmt.Errorf("unexpected op: %s", op.Op)
		}
	}

	return nil
}

func (op *Operation) add(parent *yaml.Node, match *yaml.Node) {
	switch match.Kind {
	case yaml.ScalarNode:
		parent.Content = addChildAtIndex(parent, match, op.Value)
	case yaml.MappingNode:
		match.Content = append(match.Content, op.Value.Content[0].Content...)
	case yaml.SequenceNode:
		match.Content = append(match.Content, op.Value.Content...)
	case yaml.DocumentNode:
		match.Content[0].Content = append(match.Content[0].Content, op.Value.Content[0].Content...)
	}
}

func (op *Operation) remove(parent *yaml.Node, match *yaml.Node) {
	switch parent.Kind {
	case yaml.MappingNode:
		parent.Content = removeProperty(parent, match)
	case yaml.SequenceNode:
		parent.Content = removeChild(parent, match)
	}
}

func (op *Operation) replace(parent *yaml.Node, match *yaml.Node) {
	parent.Content = replaceChildAtIndex(parent, match, op.Value)
}

func find(doc *yaml.Node, predicate func(*yaml.Node) bool) *yaml.Node {
	if predicate(doc) {
		return doc
	}

	for _, content := range doc.Content {
		if found := find(content, predicate); found != nil {
			return found
		}
	}

	return nil
}

func containsChild(child *yaml.Node) func(*yaml.Node) bool {
	return func(node *yaml.Node) bool {
		for _, c := range node.Content {
			if c == child {
				return true
			}
		}
		return false
	}
}

func childIndex(children []*yaml.Node, child *yaml.Node) int {
	for p, v := range children {
		if v == child {
			return p
		}
	}
	return -1
}

func removeProperty(parent *yaml.Node, child *yaml.Node) []*yaml.Node {
	childIndex := childIndex(parent.Content, child)
	return append(parent.Content[0:childIndex-1], parent.Content[childIndex+1:]...)
}

func removeChild(parent *yaml.Node, child *yaml.Node) []*yaml.Node {
	var remaining []*yaml.Node
	for _, current := range parent.Content {
		if child == current {
			continue
		}
		remaining = append(remaining, current)
	}
	return remaining
}

func addChildAtIndex(parent *yaml.Node, child *yaml.Node, value *yaml.Node) []*yaml.Node {
	childIdx := childIndex(parent.Content, child)
	return append(parent.Content[0:childIdx], append(value.Content, parent.Content[childIdx:]...)...)
}

func replaceChildAtIndex(parent *yaml.Node, child *yaml.Node, value *yaml.Node) []*yaml.Node {
	childIdx := childIndex(parent.Content, child)
	return append(parent.Content[0:childIdx], append(value.Content, parent.Content[childIdx+1:]...)...)
}

func createMappingNode(property string, value *yaml.Node) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.DocumentNode,
		Content: []*yaml.Node{
			{
				Kind: yaml.MappingNode,
				Tag:  "!!map",
				Content: []*yaml.Node{
					{
						Kind:  yaml.ScalarNode,
						Value: property,
						Tag:   "!!str",
					},
					value,
				},
			},
		},
	}
}

func getParents(doc *yaml.Node, path OpPath) ([]*yaml.Node, error) {
	parentPath, err := yamlpath.NewPath(path.getParentPath())
	if err != nil {
		return nil, err
	}

	parents, err := parentPath.Find(doc)
	if err != nil {
		return nil, err
	}

	return parents, nil
}
