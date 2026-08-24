package refdata

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// The dataset recognises an upstream model id with a small boolean expression:
// a leaf that compares the string, or `or`/`and` over further clauses.
//
// Every clause form the dataset can express is implemented here, and an
// unrecognised one is a parse error rather than a clause that never matches.
// The two are indistinguishable at the point of use -- both simply fail to
// recognise anything -- and the difference only surfaces as "half our models
// stopped being priced", weeks later, with nothing pointing at the cause.
type clause interface {
	matches(s string) bool
	// namesExactly reports whether this clause accepts s because the dataset
	// spells that name out, rather than because a prefix or substring rule
	// happens to cover it.
	namesExactly(s string) bool
}

type equalsClause string

func (c equalsClause) matches(s string) bool      { return string(c) == s }
func (c equalsClause) namesExactly(s string) bool { return string(c) == s }

type containsClause string

func (c containsClause) matches(s string) bool    { return strings.Contains(s, string(c)) }
func (c containsClause) namesExactly(string) bool { return false }

type startsWithClause string

func (c startsWithClause) matches(s string) bool    { return strings.HasPrefix(s, string(c)) }
func (c startsWithClause) namesExactly(string) bool { return false }

type endsWithClause string

func (c endsWithClause) matches(s string) bool    { return strings.HasSuffix(s, string(c)) }
func (c endsWithClause) namesExactly(string) bool { return false }

type regexClause struct{ re *regexp.Regexp }

func (c regexClause) matches(s string) bool    { return c.re.MatchString(s) }
func (c regexClause) namesExactly(string) bool { return false }

type orClause []clause

func (c orClause) matches(s string) bool {
	for _, sub := range c {
		if sub.matches(s) {
			return true
		}
	}
	return false
}

func (c orClause) namesExactly(s string) bool {
	for _, sub := range c {
		if sub.namesExactly(s) {
			return true
		}
	}
	return false
}

type andClause []clause

func (c andClause) matches(s string) bool {
	if len(c) == 0 {
		return false
	}
	for _, sub := range c {
		if !sub.matches(s) {
			return false
		}
	}
	return true
}

func (c andClause) namesExactly(s string) bool {
	if !c.matches(s) {
		return false
	}
	for _, sub := range c {
		if sub.namesExactly(s) {
			return true
		}
	}
	return false
}

// parseClause reads one clause. A clause object carries exactly one key; more
// than one would be an expression whose meaning the dataset does not define, so
// it is refused rather than resolved by whichever key the decoder saw first.
func parseClause(raw json.RawMessage) (clause, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("the entry has no match rule")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("read the match rule: %w", err)
	}
	if len(obj) != 1 {
		return nil, fmt.Errorf("a match rule must carry exactly one operator, found %d", len(obj))
	}
	for op, arg := range obj {
		switch op {
		case "equals", "contains", "starts_with", "ends_with", "regex":
			var lit string
			if err := json.Unmarshal(arg, &lit); err != nil {
				return nil, fmt.Errorf("match rule %q: %w", op, err)
			}
			return leafClause(op, lit)
		case "or", "and":
			var subs []json.RawMessage
			if err := json.Unmarshal(arg, &subs); err != nil {
				return nil, fmt.Errorf("match rule %q: %w", op, err)
			}
			out := make([]clause, 0, len(subs))
			for _, sub := range subs {
				c, err := parseClause(sub)
				if err != nil {
					return nil, err
				}
				out = append(out, c)
			}
			if op == "or" {
				return orClause(out), nil
			}
			return andClause(out), nil
		default:
			return nil, fmt.Errorf("unknown match operator %q", op)
		}
	}
	return nil, fmt.Errorf("the entry has no match rule")
}

func leafClause(op, lit string) (clause, error) {
	switch op {
	case "equals":
		return equalsClause(lit), nil
	case "contains":
		return containsClause(lit), nil
	case "starts_with":
		return startsWithClause(lit), nil
	case "ends_with":
		return endsWithClause(lit), nil
	default: // regex
		re, err := regexp.Compile(lit)
		if err != nil {
			return nil, fmt.Errorf("match rule %q is not a usable expression: %w", lit, err)
		}
		return regexClause{re: re}, nil
	}
}
