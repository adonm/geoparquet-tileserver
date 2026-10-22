// Package filter compiles OGC API Features / CQL2 filter expressions and
// evaluates them against a feature's scalar properties.
package filter

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	cql2 "github.com/exergy-dev/go-cql2"
	_ "github.com/exergy-dev/go-cql2/codecs"
)

type Filter struct {
	Raw  string
	root cql2.Node
}

func Compile(text string) (*Filter, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	n, err := cql2.Parse([]byte(text))
	if err != nil {
		return nil, err
	}
	return &Filter{Raw: text, root: n}, nil
}

func (f *Filter) Match(props map[string]any) (bool, error) {
	if f == nil {
		return true, nil
	}
	v, err := eval(f.root, props)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	return ok && b, nil
}

func eval(n cql2.Node, p map[string]any) (any, error) {
	switch x := n.(type) {
	case *cql2.PropertyRef:
		return p[x.Name], nil
	case *cql2.StringLit:
		return x.Value, nil
	case *cql2.BoolLit:
		return x.Value, nil
	case *cql2.NullLit:
		return nil, nil
	case *cql2.NumLit:
		if i, err := x.Value.Int64(); err == nil {
			return i, nil
		}
		f, err := x.Value.Float64()
		if err != nil {
			return nil, err
		}
		return f, nil
	case *cql2.ArrayLit:
		out := make([]any, 0, len(x.Elements))
		for _, e := range x.Elements {
			v, err := eval(e, p)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case *cql2.Op:
		return evalOp(x, p)
	default:
		return nil, fmt.Errorf("unsupported CQL2 node %T", n)
	}
}

func evalOp(op *cql2.Op, p map[string]any) (any, error) {
	args := make([]any, 0, len(op.Args))
	for _, a := range op.Args {
		v, err := eval(a, p)
		if err != nil {
			return nil, err
		}
		args = append(args, v)
	}
	switch op.Op {
	case cql2.OpAnd:
		for _, v := range args {
			if b, _ := v.(bool); !b {
				return false, nil
			}
		}
		return true, nil
	case cql2.OpOr:
		for _, v := range args {
			if b, _ := v.(bool); b {
				return true, nil
			}
		}
		return false, nil
	case cql2.OpNot:
		b, _ := args[0].(bool)
		return !b, nil
	case cql2.OpEq, cql2.OpNeq, cql2.OpLt, cql2.OpLte, cql2.OpGt, cql2.OpGte:
		return compare(op.Op, args[0], args[1])
	case cql2.OpLike:
		s, _ := args[0].(string)
		pat, _ := args[1].(string)
		re := regexp.MustCompile("^" + likeToRegexp(pat) + "$")
		return re.MatchString(s), nil
	case cql2.OpBetween:
		lo, err := compare(cql2.OpGte, args[0], args[1])
		if err != nil {
			return nil, err
		}
		hi, err := compare(cql2.OpLte, args[0], args[2])
		if err != nil {
			return nil, err
		}
		return lo && hi, nil
	case cql2.OpIn:
		list, _ := args[1].([]any)
		for _, v := range list {
			if equal(args[0], v) {
				return true, nil
			}
		}
		return false, nil
	case cql2.OpIsNull:
		return args[0] == nil, nil
	default:
		return nil, fmt.Errorf("unsupported CQL2 operator %q", op.Op)
	}
}

func compare(op cql2.Operator, a, b any) (bool, error) {
	if a == nil || b == nil {
		return false, nil
	}
	if ab, ok := a.(bool); ok {
		bb, ok := b.(bool)
		if !ok {
			return false, fmt.Errorf("cannot compare boolean with %T", b)
		}
		switch op {
		case cql2.OpEq:
			return ab == bb, nil
		case cql2.OpNeq:
			return ab != bb, nil
		}
		return false, fmt.Errorf("unsupported boolean comparison %q", op)
	}
	if af, ok := num(a); ok {
		bf, ok := num(b)
		if !ok {
			return false, fmt.Errorf("cannot compare number with %T", b)
		}
		switch op {
		case cql2.OpEq:
			return af == bf, nil
		case cql2.OpNeq:
			return af != bf, nil
		case cql2.OpLt:
			return af < bf, nil
		case cql2.OpLte:
			return af <= bf, nil
		case cql2.OpGt:
			return af > bf, nil
		case cql2.OpGte:
			return af >= bf, nil
		}
	}
	as, aok := a.(string)
	bs, bok := b.(string)
	if !aok || !bok {
		return false, fmt.Errorf("cannot compare %T with %T", a, b)
	}
	switch op {
	case cql2.OpEq:
		return as == bs, nil
	case cql2.OpNeq:
		return as != bs, nil
	case cql2.OpLt:
		return as < bs, nil
	case cql2.OpLte:
		return as <= bs, nil
	case cql2.OpGt:
		return as > bs, nil
	case cql2.OpGte:
		return as >= bs, nil
	}
	return false, fmt.Errorf("unsupported comparison %q", op)
}

func equal(a, b any) bool {
	if af, ok := num(a); ok {
		bf, ok := num(b)
		return ok && af == bf
	}
	return a == b
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func likeToRegexp(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return b.String()
}
