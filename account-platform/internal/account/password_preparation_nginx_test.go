package account

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Test-only directive/block subset. This is not nginx engine validation.
// Token-boundary comments and quote/escape handling follow nginx's
// release-1.27.5 src/core/ngx_conf_file.c:473-768. Malformed input fails closed.
type preparationNginxNode struct {
	args     []string
	children []preparationNginxNode
	block    bool
}

func preparationNginxConfig(text string) ([]preparationNginxNode, error) {
	type token struct {
		kind byte
		word string
	}
	space := func(c byte) bool { return strings.ContainsRune(" \t\r\n", rune(c)) }
	var tokens []token
	for at := 0; at < len(text); {
		c := text[at]
		if space(c) {
			at++
			continue
		}
		if c == '#' {
			for at < len(text) && text[at] != '\n' {
				at++
			}
			continue
		}
		if strings.ContainsRune("{};", rune(c)) {
			tokens = append(tokens, token{kind: c})
			at++
			continue
		}
		var quote byte
		if c == '\'' || c == '"' {
			quote = c
			at++
		}
		closed := quote == 0
		var word strings.Builder
		for at < len(text) {
			c = text[at]
			if c == '\\' {
				at++
				if at == len(text) {
					return nil, fmt.Errorf("dangling nginx escape")
				}
				next := text[at]
				at++
				switch next {
				case 't':
					word.WriteByte('\t')
				case 'r':
					word.WriteByte('\r')
				case 'n':
					word.WriteByte('\n')
				case '"', '\'', '\\':
					word.WriteByte(next)
				default:
					word.WriteByte('\\')
					word.WriteByte(next)
				}
			} else if quote != 0 && c == quote {
				at++
				closed = true
				break
			} else if quote == 0 && (space(c) || strings.ContainsRune("{};", rune(c))) {
				break
			} else {
				word.WriteByte(c)
				at++
			}
		}
		if !closed || (quote != 0 && at < len(text) && !space(text[at]) && text[at] != ';' && text[at] != '{' && text[at] != ')') {
			return nil, fmt.Errorf("invalid nginx quote boundary")
		}
		tokens = append(tokens, token{kind: 'w', word: word.String()})
	}
	pos := 0
	var level func(bool) ([]preparationNginxNode, error)
	level = func(nested bool) ([]preparationNginxNode, error) {
		var result []preparationNginxNode
		for pos < len(tokens) {
			if tokens[pos].kind == '}' {
				if !nested {
					return nil, fmt.Errorf("unexpected nginx block end")
				}
				pos++
				return result, nil
			}
			node := preparationNginxNode{}
			for pos < len(tokens) && tokens[pos].kind == 'w' {
				node.args = append(node.args, tokens[pos].word)
				pos++
			}
			if len(node.args) == 0 || pos == len(tokens) || (tokens[pos].kind != ';' && tokens[pos].kind != '{') {
				return nil, fmt.Errorf("unterminated nginx directive")
			}
			node.block = tokens[pos].kind == '{'
			pos++
			if node.block {
				var err error
				node.children, err = level(true)
				if err != nil {
					return nil, err
				}
			}
			result = append(result, node)
		}
		if nested {
			return nil, fmt.Errorf("unterminated nginx block")
		}
		return result, nil
	}
	return level(false)
}

func preparationNginxSources(text string, location []string) ([]string, error) {
	nodes, err := preparationNginxConfig(text)
	if err != nil {
		return nil, err
	}
	var matches [][]preparationNginxNode
	var visit func([]preparationNginxNode)
	visit = func(nodes []preparationNginxNode) {
		for _, node := range nodes {
			if node.block && reflect.DeepEqual(node.args, location) {
				matches = append(matches, node.children)
			}
			visit(node.children)
		}
	}
	visit(nodes)
	if len(matches) != 1 {
		return nil, fmt.Errorf("missing or ambiguous nginx location")
	}
	result := []string{}
	for _, node := range matches[0] {
		if !node.block && len(node.args) >= 2 && node.args[0] == "proxy_set_header" && node.args[1] == "X-Pinvou-Client-Source" {
			if len(node.args) != 3 {
				return nil, fmt.Errorf("invalid source header arity")
			}
			result = append(result, node.args[2])
		}
	}
	return result, nil
}

func TestPasswordPreparationNginxSyntax(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "nginx-source-syntax.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Text string
		Sources    []string
		Error      bool
	}
	if err = json.Unmarshal(raw, &cases); err != nil || len(cases) != 9 {
		t.Fatal("invalid syntax oracle", err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			got, err := preparationNginxSources(c.Text, []string{"location", "/fixture"})
			if c.Error {
				if err == nil {
					t.Fatal("malformed syntax accepted")
				}
			} else if err != nil || !reflect.DeepEqual(got, c.Sources) {
				t.Fatalf("source directives: got %q err=%v; want %q", got, err, c.Sources)
			}
		})
	}
}
