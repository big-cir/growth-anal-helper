package pipeline

import (
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	_ "github.com/pingcap/tidb/pkg/parser/test_driver" // literal values for the parser

	"growth-lab-cdc/internal/cdc"
)

// judgeStatement decides whether run may pass over a statement-format event.
// It returns nil only for account and privilege statements, and for data changes
// whose every table is in a system database. Anything else stops the run.
func judgeStatement(p *parser.Parser, defaultDB, q string) error {
	stmts, _, err := p.Parse(q, "", "")
	if err != nil || len(stmts) == 0 {
		return &cdc.StopError{Msg: "a statement the SQL parser cannot read"}
	}
	for _, s := range stmts {
		switch s.(type) {
		case *ast.GrantStmt, *ast.RevokeStmt, *ast.CreateUserStmt, *ast.AlterUserStmt, *ast.DropUserStmt, *ast.SetPwdStmt, *ast.FlushStmt:
			continue
		case *ast.InsertStmt, *ast.UpdateStmt, *ast.DeleteStmt, *ast.LoadDataStmt:
			var c tableCollector
			s.Accept(&c)
			if len(c.names) == 0 {
				return &cdc.StopError{Msg: "a statement-format data change whose tables cannot be read"}
			}
			for _, t := range c.names {
				db := t.Schema.O
				if db == "" {
					db = defaultDB
				}
				if !cdc.IsSystemDatabase(db) {
					return &cdc.StopError{Msg: "a statement-format data change outside the system databases (the copy cannot apply it)"}
				}
			}
		default:
			return &cdc.StopError{Msg: "a statement-format event of a kind run does not pass over"}
		}
	}
	return nil
}

type tableCollector struct{ names []*ast.TableName }

func (c *tableCollector) Enter(n ast.Node) (ast.Node, bool) {
	if t, ok := n.(*ast.TableName); ok {
		c.names = append(c.names, t)
	}
	return n, false
}

func (c *tableCollector) Leave(n ast.Node) (ast.Node, bool) { return n, true }
