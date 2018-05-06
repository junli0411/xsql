/**
 *  author: lim
 *  data  : 18-5-6 下午8:56
 */

package midconn


import (
	"github.com/lemonwx/xsql/sqlparser"
	"fmt"
)



type Stmt struct {
	id uint32

	cliParams  int
	nodeParams int

	cliColumns uint16
	nodeColumns uint16

	cliArgs []interface{}
	nodeArgs []interface{}

	s sqlparser.Statement

	sql string
	originSql string

	stmtIdMeta map[int]uint32

	//forUpdateStmts []*Stmt
	forUpdateStmts map[int]*Stmt
	forUpStmtIdMeta map[int]uint32
	forUpdateSql string
}

func NewStmt() *Stmt {
	stmt := new(Stmt)
	stmt.stmtIdMeta = make(map[int]uint32)
	stmt.forUpdateStmts = make(map[int]*Stmt)
	stmt.forUpStmtIdMeta = make(map[int]uint32)

	stmt.nodeParams = -1

	return stmt
}


func (s *Stmt) InitParams() {
	s.cliArgs = make([]interface{}, s.cliParams)
	s.nodeArgs = make([]interface{}, s.nodeParams)
}

func (s *Stmt) ResetParams(size int) {
	for idx := 0; idx < s.cliParams; idx += 1 {
		s.cliArgs[idx] = nil
	}
	for idx := 0; idx < s.nodeParams; idx += 1 {
		s.nodeArgs[idx] = nil
	}
}


func (s *Stmt) ChkEqual(params int, columns uint16) error {
	if s.nodeParams == -1 {
		// first prepare
		s.nodeParams = params
		s.nodeColumns = columns
		return nil
	} else {
		if s.nodeParams == params && s.nodeColumns == columns {
			return nil
		}
	}

	return fmt.Errorf("exec prepare between multi nodes ret not equal")
}