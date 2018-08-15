/**
 *  author: lim
 *  data  : 18-4-6 下午5:15
 */

package midconn

import (
	"strconv"

	"sync"

	"fmt"

	"github.com/lemonwx/log"
	"github.com/lemonwx/xsql/mysql"
	"github.com/lemonwx/xsql/sqlparser"
)

func (conn *MidConn) handleShow(stmt *sqlparser.Show, sql string) error {
	// show only send to one node
	back, err := conn.pools[0].GetConn(conn.db)
	if err != nil {
		return err
	}
	defer conn.pools[0].PutConn(back)

	ret, err := back.Execute(mysql.COM_QUERY, []byte(sql))
	if err != nil {
		return err
	}
	return conn.HandleSelRets([]*mysql.Result{ret})
	/*
		rets, err := conn.ExecuteMultiNode(mysql.COM_QUERY, []byte(sql), []int{0})
		if err != nil {
			log.Errorf("execute in multi node failed: %v", err)
			return err
		}

		return conn.HandleSelRets(rets)
	*/

}

func (conn *MidConn) handleSimpleSelect(stmt *sqlparser.SimpleSelect, sql string) error {
	log.Debugf("[%d] handle simple select", conn.ConnectionId)

	back, err := conn.pools[0].GetConn(conn.db)
	if err != nil {
		return err
	}
	defer conn.pools[0].PutConn(back)

	ret, err := back.Execute(mysql.COM_QUERY, []byte(sql))
	if err != nil {
		return err
	}
	return conn.HandleSelRets([]*mysql.Result{ret})
}

func (conn *MidConn) handleSelect(stmt *sqlparser.Select) ([]*mysql.Result, error) {
	var err error

	plan, err := conn.getPlan(stmt)
	if err != nil {
		return nil, err
	}
	conn.nodeIdx = plan.ShardList

	if len(conn.nodeIdx) == 0 {
		r := conn.newEmptyResultset(stmt)
		return []*mysql.Result{&mysql.Result{Resultset: r}}, nil
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var vRet interface{}
	var vInUse map[uint64]uint8
	var rets []*mysql.Result
	var exeErr error

	go func() {
		vRet = conn.getCurVInUse()
		wg.Done()
	}()

	go func() {
		newSql := sqlparser.String(stmt)
		rets, exeErr = conn.ExecuteOnNodePool([]byte(newSql), plan.ShardList)
		//rets, exeErr = conn.ExecuteMultiNode(mysql.COM_QUERY, []byte(newSql), conn.nodeIdx)
		wg.Done()
	}()
	wg.Wait()

	switch vv := vRet.(type) {
	case error:
		return nil, err
	case map[uint64]uint8:
		vInUse = vv
	default:
		return nil, fmt.Errorf("unexpected error from getCurVInUse")
	}

	extraSz := len(stmt.ExtraCols)
	for idx, ret := range rets {
		ret.Fields = ret.Fields[extraSz:]

		for rowIdx, _ := range ret.RowDatas {
			if err := conn.hideExtraCols(&ret.RowDatas[rowIdx], extraSz, vInUse); err != nil {
				return nil, err
			}
		}
		rets[idx] = ret
	}

	return rets, err
}

func (conn *MidConn) ExecuteOnSinglePool(sql []byte, nodeIdxs []int) ([]*mysql.Result, error) {
	if len(nodeIdxs) != 1 {
		return nil, fmt.Errorf("len of nodeIdxs must be 1")
	}

	idx := nodeIdxs[0]
	back, err := conn.getSingleBackConn(idx)
	if err != nil {
		return nil, err
	}

	ret, err := back.Execute(mysql.COM_QUERY, sql)
	if err != nil {
		return nil, err
	}
	return []*mysql.Result{ret}, nil
}

func (conn *MidConn) ExecuteOnMultiPool(sql []byte, nodeIdxs []int) ([]*mysql.Result, error) {
	shardSize := len(nodeIdxs)
	rets := make([]*mysql.Result, 0, shardSize)
	errs := make([]error, 0, shardSize)

	if err := conn.getMultiBackConn(nodeIdxs); err != nil {
		return nil, err
	}

	var wg sync.WaitGroup
	wg.Add(len(nodeIdxs))

	for _, idx := range nodeIdxs {
		go func(idx int) {
			back, ok := conn.execNodes[idx]
			if !ok {
				errs = append(errs, fmt.Errorf("unexpected error, idx should in conn.execNodes"))
			}
			if ret, err := back.Execute(mysql.COM_QUERY, sql); err != nil {
				errs = append(errs, err)
			} else {
				rets = append(rets, ret)
			}
			wg.Done()
		}(idx)
	}
	wg.Wait()

	switch {
	case len(errs) == shardSize:
		return nil, errs[0]
	case len(rets) == shardSize:
		return rets, nil
	default:
		return nil, fmt.Errorf("unexpected multi node return not equal")
	}
}

func (conn *MidConn) ExecuteOnNodePool(sql []byte, nodeIdxs []int) ([]*mysql.Result, error) {
	if len(nodeIdxs) == 1 {
		return conn.ExecuteOnSinglePool(sql, nodeIdxs)
	} else {
		return conn.ExecuteOnMultiPool(sql, nodeIdxs)
	}
}

func (conn *MidConn) hideExtraCols(data *mysql.RowData, size int, vs map[uint64]uint8) error {
	idx := uint8(0)
	for count := 0; count < size; count += 1 {
		s := idx + 1
		e := s + (*data)[idx]

		vStr := string((*data)[s:e])
		res, err := strconv.ParseUint(vStr, 10, 64)
		if err != nil {
			log.Errorf("[%d] ParseUint from %v failed: %v", vStr, err)
			return mysql.NewDefaultError(mysql.MID_ER_HIDE_EXTRA_FAILED)
		}
		if _, ok := vs[res]; ok {
			err = mysql.NewDefaultError(mysql.MID_ER_ROWS_IN_USE_BY_OTHER_SESSION)
			log.Errorf("[%d] hide extra col failed: %v", conn.ConnectionId, err)
			return err
		}
		idx = (*data)[idx] + idx + 1
	}
	(*data) = (*data)[idx:]
	return nil
}

func (conn *MidConn) handleLimit(rets []*mysql.Result, limit *sqlparser.Limit) ([]*mysql.Result, error) {

	if len(rets) == 0 {
		log.Errorf("[%d] handle limit rets's len == 0, unexpected err", conn.ConnectionId)
		return nil, mysql.NewDefaultError(mysql.MID_ER_UNEXPECTED)
	}

	if limit != nil {
		if limit.Offset != nil {
			log.Errorf("[%d] offset : %v not nil, not support this sql now", conn.ConnectionId, limit.Offset)
			return nil, mysql.NewDefaultError(mysql.MID_ER_UNSUPPORTED_SQL)
		}
		log.Debugf("[%d] offset: %v, rows count: %d", conn.ConnectionId, limit.Offset, limit.Rowcount)

		limitCount, err := strconv.ParseUint(string(limit.Rowcount.(sqlparser.NumVal)), 10, 64)
		if err != nil {
			log.Errorf("[%d] parse limit count failed: %v", conn.ConnectionId, err)
			return nil, err
		}

		allCount := uint64(0)
		for idx, ret := range rets {
			tmp := uint64(len(ret.RowDatas))
			if allCount+tmp >= limitCount {

				rets[idx].RowDatas = rets[idx].RowDatas[:limitCount-allCount]
				return rets[:idx+1], nil
			}
			allCount += tmp
		}
	}

	return rets, nil
}

func (conn *MidConn) setupNodeStatus(vInUse map[uint64]byte, hide bool, isStmt bool, extraSize int) {
	for idx, _ := range conn.nodes {
		conn.nodes[idx].VersionsInUse = vInUse
		conn.nodes[idx].NeedHide = hide
		conn.nodes[idx].IsStmt = isStmt
		conn.nodes[idx].ExtraSize = extraSize
	}
}
