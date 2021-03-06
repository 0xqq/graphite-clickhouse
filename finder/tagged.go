package finder

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/go-graphite/carbonapi/pkg/parser"
	"github.com/lomik/graphite-clickhouse/helper/clickhouse"
)

type TaggedTermOp int

const (
	TaggedTermEq       TaggedTermOp = 1
	TaggedTermMatch    TaggedTermOp = 2
	TaggedTermNe       TaggedTermOp = 3
	TaggedTermNotMatch TaggedTermOp = 4
)

type TaggedTerm struct {
	Key   string
	Op    TaggedTermOp
	Value string
}

type TaggedTermList []TaggedTerm

func (s TaggedTermList) Len() int {
	return len(s)
}
func (s TaggedTermList) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
func (s TaggedTermList) Less(i, j int) bool {
	if s[i].Op < s[j].Op {
		return true
	}
	if s[i].Op > s[j].Op {
		return false
	}
	if s[i].Key == "__name__" && s[j].Key != "__name__" {
		return true
	}
	return false
}

type TaggedFinder struct {
	url   string             // clickhouse dsn
	table string             // graphite_tag table
	opts  clickhouse.Options // clickhouse query timeout
	body  []byte             // clickhouse response
}

func NewTagged(url string, table string, opts clickhouse.Options) *TaggedFinder {
	return &TaggedFinder{
		url:   url,
		table: table,
		opts:  opts,
	}
}

func TaggedTermWhere1(term *TaggedTerm) string {
	// positive expression check only in Tag1
	// negative check in all Tags
	switch term.Op {
	case TaggedTermEq:
		return fmt.Sprintf("Tag1=%s", Qf("%s=%s", term.Key, term.Value))
	case TaggedTermNe:
		return fmt.Sprintf("NOT arrayExists((x) -> x=%s, Tags)", Qf("%s=%s", term.Key, term.Value))
	case TaggedTermMatch:
		return fmt.Sprintf(
			"(Tag1 LIKE %s) AND (match(Tag1, %s))",
			Qf("%s=%%", term.Key),
			Qf("%s=%s", term.Key, term.Value),
		)
	case TaggedTermNotMatch:
		return fmt.Sprintf(
			"NOT arrayExists((x) -> (x LIKE %s) AND (match(x, %s)), Tags)",
			Qf("%s=%%", term.Key),
			Qf("%s=%s", term.Key, term.Value),
		)
	default:
		return ""
	}
}

func TaggedTermWhereN(term *TaggedTerm) string {
	// arrayExists((x) -> %s, Tags)
	switch term.Op {
	case TaggedTermEq:
		return fmt.Sprintf("arrayExists((x) -> x=%s, Tags)", Qf("%s=%s", term.Key, term.Value))
	case TaggedTermNe:
		return fmt.Sprintf("NOT arrayExists((x) -> x=%s, Tags)", Qf("%s=%s", term.Key, term.Value))
	case TaggedTermMatch:
		return fmt.Sprintf(
			"arrayExists((x) -> (x LIKE %s) AND (match(x, %s)), Tags)",
			Qf("%s=%%", term.Key),
			Qf("%s=%s", term.Key, term.Value),
		)
	case TaggedTermNotMatch:
		return fmt.Sprintf(
			"NOT arrayExists((x) -> (x LIKE %s) AND (match(x, %s)), Tags)",
			Qf("%s=%%", term.Key),
			Qf("%s=%s", term.Key, term.Value),
		)
	default:
		return ""
	}
}

func MakeTaggedWhere(expr []string) (string, error) {
	terms := make([]TaggedTerm, len(expr))

	for i := 0; i < len(expr); i++ {
		s := expr[i]

		a := strings.SplitN(s, "=", 2)
		if len(a) != 2 {
			return "", fmt.Errorf("wrong seriesByTag expr: %#v", s)
		}

		a[0] = strings.TrimSpace(a[0])
		a[1] = strings.TrimSpace(a[1])

		op := "="

		if len(a[0]) > 0 && a[0][len(a[0])-1] == '!' {
			op = "!" + op
			a[0] = strings.TrimSpace(a[0][:len(a[0])-1])
		}

		if len(a[1]) > 0 && a[1][0] == '~' {
			op = op + "~"
			a[1] = strings.TrimSpace(a[1][1:])
		}

		terms[i].Key = a[0]
		terms[i].Value = a[1]

		if terms[i].Key == "name" {
			terms[i].Key = "__name__"
		}

		switch op {
		case "=":
			terms[i].Op = TaggedTermEq
		case "!=":
			terms[i].Op = TaggedTermNe
		case "=~":
			terms[i].Op = TaggedTermMatch
		case "!=~":
			terms[i].Op = TaggedTermNotMatch
		default:
			return "", fmt.Errorf("wrong seriesByTag expr: %#v", s)
		}
	}

	sort.Sort(TaggedTermList(terms))

	w := NewWhere()
	w.And(TaggedTermWhere1(&terms[0]))

	for i := 1; i < len(terms); i++ {
		w.And(TaggedTermWhereN(&terms[i]))
	}

	return w.String(), nil
}

func (t *TaggedFinder) makeWhere(query string) (string, error) {
	expr, _, err := parser.ParseExpr(query)
	if err != nil {
		return "", err
	}

	validationError := fmt.Errorf("wrong seriesByTag call: %#v", query)

	// check
	if !expr.IsFunc() {
		return "", validationError
	}
	if expr.Target() != "seriesByTag" {
		return "", validationError
	}

	args := expr.Args()
	if len(args) < 1 {
		return "", validationError
	}

	for i := 0; i < len(args); i++ {
		if !args[i].IsString() {
			return "", validationError
		}
	}

	conditions := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		s := args[i].StringValue()
		if s == "" {
			continue
		}
		conditions = append(conditions, s)
	}

	return MakeTaggedWhere(conditions)
}

func (t *TaggedFinder) Execute(ctx context.Context, query string, from int64, until int64) error {
	w, err := t.makeWhere(query)
	if err != nil {
		return err
	}

	dateWhere := NewWhere()
	dateWhere.Andf(
		"Date >='%s' AND Date <= '%s'",
		time.Unix(from, 0).Format("2006-01-02"),
		time.Unix(until, 0).Format("2006-01-02"),
	)

	sql := fmt.Sprintf("SELECT Path FROM %s WHERE (%s) AND (%s) AND (Deleted=0) GROUP BY Path", t.table, dateWhere.String(), w)
	t.body, err = clickhouse.Query(ctx, t.url, sql, t.table, t.opts)
	return err
}

func (t *TaggedFinder) List() [][]byte {
	if t.body == nil {
		return [][]byte{}
	}

	rows := bytes.Split(t.body, []byte{'\n'})

	skip := 0
	for i := 0; i < len(rows); i++ {
		if len(rows[i]) == 0 {
			skip++
			continue
		}
		if skip > 0 {
			rows[i-skip] = rows[i]
		}
	}

	rows = rows[:len(rows)-skip]

	return rows
}

func (t *TaggedFinder) Series() [][]byte {
	return t.List()
}

func (t *TaggedFinder) Abs(v []byte) []byte {
	u, err := url.Parse(string(v))
	if err != nil {
		return v
	}

	tags := make([]string, 0, len(u.Query()))
	for k, v := range u.Query() {
		tags = append(tags, fmt.Sprintf("%s=%s", k, v[0]))
	}

	sort.Strings(tags)
	if len(tags) == 0 {
		return []byte(u.Path)
	}

	return []byte(fmt.Sprintf("%s;%s", u.Path, strings.Join(tags, ";")))
}
