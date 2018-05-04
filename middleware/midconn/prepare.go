/**
 *  author: lim
 *  data  : 18-4-24 下午9:57
 */

package midconn

import (
	"github.com/lemonwx/log"
	"github.com/lemonwx/xsql/sqlparser"
	"github.com/lemonwx/xsql/mysql"
	"strings"
	"fmt"
	"encoding/binary"
	"strconv"
	"math"
	"io"
	"github.com/lemonwx/xsql/middleware/meta"
	"utils"
	"time"
)


var paramFieldData []byte = (&mysql.Field{}).Dump()
var columnFieldData []byte = (&mysql.Field{}).Dump()

type Stmt struct {
	id uint32
	ids []uint32

	cliParams  int
	nodeParams int

	cliColumns uint16
	nodeColumns uint16

	cliArgs []interface{}
	nodeArgs []interface{}

	s sqlparser.Statement
	forUpdate *sqlparser.Select

	sql string

	nodeIdx []int
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

func (conn *MidConn) writePrepare(s *Stmt) error {
	data := make([]byte, 4, 128)

	//status ok
	data = append(data, 0)
	//stmt id
	data = append(data, mysql.Uint32ToBytes(s.id)...)
	//number columns
	data = append(data, mysql.Uint16ToBytes(uint16(s.cliColumns))...)
	//number params
	data = append(data, mysql.Uint16ToBytes(uint16(s.cliParams))...)
	//filter [00]
	data = append(data, 0)
	//warning count
	data = append(data, 0, 0)

	if err := conn.cli.WritePacket(data); err != nil {
		return err
	}

	if s.cliParams > 0 {
		for i := 0; i < s.cliParams; i++ {
			data = data[0:4]
			data = append(data, paramFieldData...)

			if err := conn.cli.WritePacket(data); err != nil {
				return err
			}
		}

		if err := conn.cli.WriteEOF(conn.status[0]); err != nil {
			return err
		}
	}

	if s.cliColumns > 0 {
		for i := uint16(0); i < s.cliColumns; i++ {
			data = data[0:4]
			data = append(data, columnFieldData...)

			if err := conn.cli.WritePacket(data); err != nil {
				return err
			}
		}

		if err := conn.cli.WriteEOF(conn.status[0]); err != nil {
			return err
		}

	}
	return nil
}

func (conn *MidConn) myPrepare(sql string, idx int) (*Stmt, error) {
	var err error

	stmt := new(Stmt)

	sql = strings.TrimRight(sql, ";")
	if stmt.s, err = sqlparser.Parse(sql); err != nil {
		return nil, fmt.Errorf(`parse sql "%s" error`, sql)
	}

	stmt.sql = sqlparser.String(stmt.s)

	// send prepare to node[0]
	if err = conn.nodes[idx].ExecutePrepare([]byte(stmt.sql), &stmt.id, &stmt.nodeColumns, &stmt.nodeParams); err != nil {
		log.Debugf("[%d] send prepare sql to %d faild: %v", conn.ConnectionId, idx, err)
		return nil, err
	}
	stmt.nodeIdx = []int{0}
	stmt.ids = []uint32{stmt.id}

	// handle cliparams and node params
	if _, ok := stmt.s.(*sqlparser.Select); ok {
		stmt.cliParams = stmt.nodeParams
		stmt.cliColumns = stmt.cliColumns - 1
	} else {
		stmt.cliParams = stmt.nodeParams - 1
	}

	conn.stmts[stmt.id] = stmt
	stmt.InitParams()

	return stmt, nil
}


func (conn *MidConn) handlePrepare(sql string) error {
	log.Debugf("[%d] handle prepare %s", conn.ConnectionId, sql)

	var err error

	if conn.db == "" {
		return mysql.NewDefaultError(mysql.ER_NO_DB_ERROR)
	}

	stmt, err := conn.myPrepare(sql, 0)
	if err != nil {
		return err
	}

	// send prepare result to mysql cli
	if err = conn.writePrepare(stmt); err != nil {
		return err
	}

	return nil
}

func (conn *MidConn) handleStmtExecute(data []byte) error {
	log.Debugf("[%d] handle stmt execute %v", conn.ConnectionId, data)

	var err error

	if len(data) < 9 {
		return mysql.ErrMalformPacket
	}

	pos := 0
	id := binary.LittleEndian.Uint32(data[0:4])
	pos += 4

	s, ok := conn.stmts[id]
	if !ok {
		return mysql.NewDefaultError(mysql.ER_UNKNOWN_STMT_HANDLER,
			strconv.FormatUint(uint64(id), 10), "stmt_execute")
	}

	flag := data[pos]
	pos++
	//now we only support CURSOR_TYPE_NO_CURSOR flag
	if flag != 0 {
		return mysql.NewError(mysql.ER_UNKNOWN_ERROR, fmt.Sprintf("unsupported flag %d", flag))
	}

	//skip iteration-count, always 1
	pos += 4

	var nullBitmaps []byte
	var paramTypes []byte
	var paramValues []byte

	paramNum := s.cliParams

	if paramNum > 0 {
		nullBitmapLen := (s.cliParams + 7) >> 3
		if len(data) < (pos + nullBitmapLen + 1) {
			return mysql.ErrMalformPacket
		}
		nullBitmaps = data[pos : pos+nullBitmapLen]
		pos += nullBitmapLen

		//new param bound flag
		if data[pos] == 1 {
			pos++
			if len(data) < (pos + (paramNum << 1)) {
				return mysql.ErrMalformPacket
			}

			paramTypes = data[pos : pos+(paramNum<<1)]
			pos += (paramNum << 1)

			paramValues = data[pos:]
		}

		if err := conn.bindStmtArgs(s, nullBitmaps, paramTypes, paramValues); err != nil {
			return err
		}
	}

	log.Debugf("[%d] prepare stmt: %v, exec: %v", conn.ConnectionId, s.cliArgs, data)

	if conn.nodeIdx, err = sqlparser.GetStmtShardListIndex(
		s.s, meta.GetRouter(conn.db), conn.makeBindVars(s.cliArgs)); err != nil {
			log.Debugf("[%d] get nodeidx failed: %v", conn.ConnectionId, err)
			return err
	}
	log.Debugf("[%d] get nodeidx %v", conn.ConnectionId, conn.nodeIdx)

	if err = conn.chkPrepare(s); err != nil {
		return err
	}

	log.Debugf("[%d] prepare stmt: %v, exec: %v", conn.ConnectionId, s, data)

	switch s.s.(type) {
	case *sqlparser.Select:
		return conn.ExecuteSelect(data)
	case *sqlparser.Insert:
		return conn.ExecuteInsert(s)
	case *sqlparser.Update:
		log.Debug(data)
		return conn.ExecuteUpdate(s)
	default:
		return UNEXPECT_MIDDLE_WARE_ERR
	}
}

func (conn *MidConn) makePkt(stmt *Stmt) []byte {
	args := stmt.nodeArgs

	const minPktLen = 4 + 1 + 4 + 1 + 4
	//mc := stmt.mc

	// Determine threshould dynamically to avoid packet size shortage.
	longDataSize := mysql.MaxPayloadLen / len(stmt.nodeArgs) + 1
	if longDataSize < 64 {
		longDataSize = 64
	}


	// Reset packet-sequence
	//mc.sequence = 0

	var data []byte = make([]byte, minPktLen)

	/*
	if len(args) == 0 {
		data = mc.buf.takeBuffer(minPktLen)
	} else {
		data = mc.buf.takeCompleteBuffer()
		fmt.Println(data[:4])
	}
	if data == nil {
		// can not take the buffer. Something must be wrong with the connection
		errLog.Print(ErrBusyBuffer)
		return errBadConnNoWrite
	}

	fmt.Println(data[:4])
	*/

	// command [1 byte]
	data[4] = mysql.COM_STMT_EXECUTE

	// statement_id [4 bytes]
	data[5] = byte(stmt.id)
	data[6] = byte(stmt.id >> 8)
	data[7] = byte(stmt.id >> 16)
	data[8] = byte(stmt.id >> 24)

	// flags (0: CURSOR_TYPE_NO_CURSOR) [1 byte]
	data[9] = 0x00

	// iteration_count (uint32(1)) [4 bytes]
	data[10] = 0x01
	data[11] = 0x00
	data[12] = 0x00
	data[13] = 0x00

	if len(args) > 0 {
		pos := minPktLen

		var nullMask []byte
		if maskLen, typesLen := (len(args)+7)/8, 1+2*len(args); pos+maskLen+typesLen >= len(data) {
			// buffer has to be extended but we don't know by how much so
			// we depend on append after all data with known sizes fit.
			// We stop at that because we deal with a lot of columns here
			// which makes the required allocation size hard to guess.
			tmp := make([]byte, pos+maskLen+typesLen)
			copy(tmp[:pos], data[:pos])
			data = tmp
			nullMask = data[pos : pos+maskLen]
			pos += maskLen
		} else {
			nullMask = data[pos : pos+maskLen]
			for i := 0; i < maskLen; i++ {
				nullMask[i] = 0
			}
			pos += maskLen
		}

		// newParameterBoundFlag 1 [1 byte]
		data[pos] = 0x01
		pos++

		// type of each parameter [len(args)*2 bytes]
		paramTypes := data[pos:]
		pos += len(args) * 2

		// value of each parameter [n bytes]
		paramValues := data[pos:pos]
		valuesCap := cap(paramValues)

		for i, arg := range args {
			// build NULL-bitmap
			if arg == nil {
				nullMask[i/8] |= 1 << (uint(i) & 7)
				paramTypes[i+i] = byte(mysql.MYSQL_TYPE_NULL)
				paramTypes[i+i+1] = 0x00
				continue
			}

			// cache types and values
			switch v := arg.(type) {
			case int64:
				paramTypes[i+i] = byte(mysql.MYSQL_TYPE_LONGLONG)
				paramTypes[i+i+1] = 0x00

				if cap(paramValues)-len(paramValues)-8 >= 0 {
					paramValues = paramValues[:len(paramValues)+8]
					binary.LittleEndian.PutUint64(
						paramValues[len(paramValues)-8:],
						uint64(v),
					)
				} else {
					paramValues = append(paramValues,
						utils.Uint64ToBytes(uint64(v))...,
					)
				}

			case float64:
				paramTypes[i+i] = byte(mysql.MYSQL_TYPE_DOUBLE)
				paramTypes[i+i+1] = 0x00

				if cap(paramValues)-len(paramValues)-8 >= 0 {
					paramValues = paramValues[:len(paramValues)+8]
					binary.LittleEndian.PutUint64(
						paramValues[len(paramValues)-8:],
						math.Float64bits(v),
					)
				} else {
					paramValues = append(paramValues,
						utils.Uint64ToBytes(math.Float64bits(v))...,
					)
				}

			case bool:
				paramTypes[i+i] = byte(mysql.MYSQL_TYPE_TINY)
				paramTypes[i+i+1] = 0x00

				if v {
					paramValues = append(paramValues, 0x01)
				} else {
					paramValues = append(paramValues, 0x00)
				}

			case []byte:
				// Common case (non-nil value) first
				if v != nil {
					paramTypes[i+i] = byte(mysql.MYSQL_TYPE_STRING)
					paramTypes[i+i+1] = 0x00

						paramValues = utils.AppendLengthEncodedInteger(paramValues,
							uint64(len(v)),
						)
						paramValues = append(paramValues, v...)
					continue
				}

				// Handle []byte(nil) as a NULL value
				nullMask[i/8] |= 1 << (uint(i) & 7)
				paramTypes[i+i] = byte(mysql.MYSQL_TYPE_NULL)
				paramTypes[i+i+1] = 0x00

			case string:
				paramTypes[i+i] = byte(mysql.MYSQL_TYPE_STRING)
				paramTypes[i+i+1] = 0x00

					paramValues = utils.AppendLengthEncodedInteger(paramValues,
						uint64(len(v)),
					)
					paramValues = append(paramValues, v...)


			case time.Time:
				paramTypes[i+i] = byte(mysql.MYSQL_TYPE_STRING)
				paramTypes[i+i+1] = 0x00

				var a [64]byte
				var b = a[:0]

				if v.IsZero() {
					b = append(b, "0000-00-00"...)
				} else {
					b = v.In(time.UTC).AppendFormat(b, mysql.TimeFormat)
				}

				paramValues = utils.AppendLengthEncodedInteger(paramValues,
					uint64(len(b)),
				)
				paramValues = append(paramValues, b...)
			}
		}

		// Check if param values exceeded the available buffer
		// In that case we must build the data packet with the new values buffer
		if valuesCap != cap(paramValues) {
			data = append(data[:pos], paramValues...)
			//mc.buf.buf = data
		}

		pos += len(paramValues)
		data = data[:pos]
	}

	return data[5:]
}

func (conn *MidConn) ExecuteUpdate(stmt *Stmt) error {
	var err error
	if err = conn.getNextVersion(); err != nil {
		return err
	}

	updateStmt := stmt.s.(*sqlparser.Update)
	sstring := sqlparser.String

	if forUpdateStmt, err := sqlparser.Parse(
		fmt.Sprintf("select version from %s %s",
			sstring(updateStmt.Table), sstring(updateStmt.Where)) );  err != nil {
		log.Debugf("[%d] Parse select for update stmt failed: %v", conn.ConnectionId, err)
	} else {
		stmt.forUpdate = forUpdateStmt.(*sqlparser.Select)
		log.Debug(stmt.forUpdate)
	}


	//stmt.nodeArgs[0] = int64(conn.NextVersion)
	stmt.nodeArgs[0] = int64(conn.NextVersion)
	copy(stmt.nodeArgs[1:], stmt.cliArgs)
	log.Debug(stmt.nodeArgs)
	newData := conn.makePkt(stmt)
	log.Debug("[1 0 0 0 0 1 0 0 0 0 1 8 0 254 0 8 0 123 0 0 0 0 0 0 0 28 108 109 109 109 109 109 109 109 109 109 109 109 109 109 109 109 109 109 109 109 109 109 109 109 109 109 109 109 80 0 0 0 0 0 0 0]")
	log.Debug(newData)

	if rets, err := conn.ExecuteMultiNode(mysql.COM_STMT_EXECUTE, newData, conn.nodeIdx); err != nil {
		return  err
	} else {
		return conn.HandleExecRets(rets)
	}
}

func (conn *MidConn) ExecuteInsert(stmt *Stmt) error {
	//1 0 0 0 0 1 0 0 0 0 1 8 0 254 0 200 0 0 0 0 0 0 0 4 110 97 109 101
	//1 0 0 0 0 1 0 0 0 0 1 8 0 8 0 254 0 57 48 0 0 0 0 0 0 200 0 0 0 0 0 0 0 4 110 97 109 101
	//1 0 0 0 0 1 0 0 0 0 1 8 0 8 0 254 0 49 212 0 0 0 0 0 0 200 0 0 0 0 0 0 0 4 110 97 109 101


	var err error
	if err = conn.getNextVersion(); err != nil {
		return err
	}

	stmt.nodeArgs[0] = int64(conn.NextVersion)
	copy(stmt.nodeArgs[1:], stmt.cliArgs)
	log.Debug(stmt.nodeArgs)

	newData := conn.makePkt(stmt)
	log.Debug("[1 0 0 0 0 1 0 0 0 0 1 8 0 8 0 254 0 57 48 0 0 0 0 0 0 10 0 0 0 0 0 0 0 4 110 97 109 101]")
	log.Debug(newData)
	log.Debug(stmt)


	if rets, err := conn.ExecuteMultiNode(mysql.COM_STMT_EXECUTE, newData, conn.nodeIdx); err != nil {
		return err
	} else {
		return conn.HandleExecRets(rets)
	}
}

func (conn *MidConn) ExecuteSelect(data []byte) error {
	var err error
	if err = conn.getVInUse(); err != nil {
		return err
	}
	conn.setupNodeStatus(conn.VersionsInUse, true, true)
	defer conn.setupNodeStatus(nil, false, false)

	if rets, err := conn.ExecuteMultiNode(mysql.COM_STMT_EXECUTE, data, conn.nodeIdx); err != nil {
		return err
	} else {
		return conn.HandleSelRets(rets)
	}
}


func (conn *MidConn) prepare(stmt *Stmt, idx int) error {



	return conn.nodes[idx].ExecutePrepare([]byte(stmt.sql), &stmt.id, &stmt.nodeColumns, &stmt.nodeParams)
}

func (conn *MidConn) chkPrepare(stmt *Stmt) error {

	if utils.CompareIntSlice(conn.nodeIdx, stmt.nodeIdx) {
		return nil
	}

	for _, idx := range conn.nodeIdx {
		if ! utils.ContainsIntSlice(stmt.nodeIdx, idx) {
			log.Debugf("[%d] node :%d need to prepare", conn.ConnectionId, idx )
			tmpStmt := new(Stmt)
			tmpStmt.s = stmt.s
			tmpStmt.sql = stmt.sql
			if err := conn.prepare(tmpStmt, idx); err != nil {
				return err
			} else {
				if tmpStmt.nodeColumns == stmt.nodeColumns && tmpStmt.nodeParams == tmpStmt.nodeParams{
					stmt.ids = append(stmt.ids, tmpStmt.id)
					stmt.nodeIdx = append(stmt.nodeIdx, idx)
				} else {
					return UNEXPECT_MIDDLE_WARE_ERR
				}
			}
		}
	}
	return nil
}

func (conn *MidConn) bindStmtArgs(s *Stmt, nullBitmap, paramTypes, paramValues []byte) error {
	args := s.cliArgs

	pos := 0

	var v []byte
	var n int = 0
	var isNull bool
	var err error

	for i := 0; i < s.cliParams; i++ {
		if nullBitmap[i>>3]&(1<<(uint(i)%8)) > 0 {
			args[i] = nil
			continue
		}

		tp := paramTypes[i<<1]
		isUnsigned := (paramTypes[(i<<1)+1] & 0x80) > 0

		switch tp {
		case mysql.MYSQL_TYPE_NULL:
			args[i] = nil
			continue

		case mysql.MYSQL_TYPE_TINY:
			if len(paramValues) < (pos + 1) {
				return mysql.ErrMalformPacket
			}

			if isUnsigned {
				args[i] = uint8(paramValues[pos])
			} else {
				args[i] = int8(paramValues[pos])
			}

			pos++
			continue

		case mysql.MYSQL_TYPE_SHORT, mysql.MYSQL_TYPE_YEAR:
			if len(paramValues) < (pos + 2) {
				return mysql.ErrMalformPacket
			}

			if isUnsigned {
				args[i] = uint16(binary.LittleEndian.Uint16(paramValues[pos : pos+2]))
			} else {
				args[i] = int16((binary.LittleEndian.Uint16(paramValues[pos : pos+2])))
			}
			pos += 2
			continue

		case mysql.MYSQL_TYPE_INT24, mysql.MYSQL_TYPE_LONG:
			if len(paramValues) < (pos + 4) {
				return mysql.ErrMalformPacket
			}

			if isUnsigned {
				args[i] = uint32(binary.LittleEndian.Uint32(paramValues[pos : pos+4]))
			} else {
				args[i] = int32(binary.LittleEndian.Uint32(paramValues[pos : pos+4]))
			}
			pos += 4
			continue

		case mysql.MYSQL_TYPE_LONGLONG:
			if len(paramValues) < (pos + 8) {
				return mysql.ErrMalformPacket
			}

			if isUnsigned {
				args[i] = binary.LittleEndian.Uint64(paramValues[pos : pos+8])
			} else {
				args[i] = int64(binary.LittleEndian.Uint64(paramValues[pos : pos+8]))
			}
			pos += 8
			continue

		case mysql.MYSQL_TYPE_FLOAT:
			if len(paramValues) < (pos + 4) {
				return mysql.ErrMalformPacket
			}

			args[i] = float32(math.Float32frombits(binary.LittleEndian.Uint32(paramValues[pos : pos+4])))
			pos += 4
			continue

		case mysql.MYSQL_TYPE_DOUBLE:
			if len(paramValues) < (pos + 8) {
				return mysql.ErrMalformPacket
			}

			args[i] = math.Float64frombits(binary.LittleEndian.Uint64(paramValues[pos : pos+8]))
			pos += 8
			continue

		case mysql.MYSQL_TYPE_DECIMAL, mysql.MYSQL_TYPE_NEWDECIMAL, mysql.MYSQL_TYPE_VARCHAR,
			mysql.MYSQL_TYPE_BIT, mysql.MYSQL_TYPE_ENUM, mysql.MYSQL_TYPE_SET, mysql.MYSQL_TYPE_TINY_BLOB,
			mysql.MYSQL_TYPE_MEDIUM_BLOB, mysql.MYSQL_TYPE_LONG_BLOB, mysql.MYSQL_TYPE_BLOB,
			mysql.MYSQL_TYPE_VAR_STRING, mysql.MYSQL_TYPE_STRING, mysql.MYSQL_TYPE_GEOMETRY,
			mysql.MYSQL_TYPE_DATE, mysql.MYSQL_TYPE_NEWDATE,
			mysql.MYSQL_TYPE_TIMESTAMP, mysql.MYSQL_TYPE_DATETIME, mysql.MYSQL_TYPE_TIME:
			if len(paramValues) < (pos + 1) {
				return mysql.ErrMalformPacket
			}

			v, isNull, n, err = LengthEnodedString(paramValues[pos:])
			pos += n
			if err != nil {
				return err
			}

			if !isNull {
				args[i] = v
				continue
			} else {
				args[i] = nil
				continue
			}
		default:
			return fmt.Errorf("Stmt Unknown FieldType %d", tp)
		}
	}
	return nil
}

func LengthEnodedString(b []byte) ([]byte, bool, int, error) {
	// Get length
	num, isNull, n := LengthEncodedInt(b)
	if num < 1 {
		return nil, isNull, n, nil
	}

	n += int(num)

	// Check data length
	if len(b) >= n {
		return b[n-int(num) : n], false, n, nil
	}
	return nil, false, n, io.EOF
}

func LengthEncodedInt(b []byte) (num uint64, isNull bool, n int) {
	switch b[0] {

	// 251: NULL
	case 0xfb:
		n = 1
		isNull = true
		return

		// 252: value of following 2
	case 0xfc:
		num = uint64(b[1]) | uint64(b[2])<<8
		n = 3
		return

		// 253: value of following 3
	case 0xfd:
		num = uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16
		n = 4
		return

		// 254: value of following 8
	case 0xfe:
		num = uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16 |
			uint64(b[4])<<24 | uint64(b[5])<<32 | uint64(b[6])<<40 |
			uint64(b[7])<<48 | uint64(b[8])<<56
		n = 9
		return
	}

	// 0-250: value of first byte
	num = uint64(b[0])
	n = 1
	return
}


/*
func (conn *MidConn) handleStmtExecute(data []byte) error {
	if len(data) < 9 {
		return UNEXPECT_MIDDLE_WARE_ERR
	}

	pos := 0
	id := binary.LittleEndian.Uint32(data[0:4])
	pos += 4

	s, ok := conn.stmts[id]
	if !ok {
		return mysql.NewDefaultError(mysql.ER_UNKNOWN_STMT_HANDLER,
			strconv.FormatUint(uint64(id), 10), "stmt_execute")
	}

	flag := data[pos]
	pos++
	//now we only support CURSOR_TYPE_NO_CURSOR flag
	if flag != 0 {
		return mysql.NewError(mysql.ER_UNKNOWN_ERROR, fmt.Sprintf("unsupported flag %d", flag))
	}

	//skip iteration-count, always 1
	pos += 4

	var nullBitmaps []byte
	var paramTypes []byte
	var paramValues []byte

	paramNum := s.params

	if paramNum > 0 {
		nullBitmapLen := (s.params + 7) >> 3
		if len(data) < (pos + nullBitmapLen + 1) {
			return mysql.ErrMalformPacket
		}
		nullBitmaps = data[pos : pos+nullBitmapLen]
		pos += nullBitmapLen

		//new param bound flag
		if data[pos] == 1 {
			pos++
			if len(data) < (pos + (paramNum << 1)) {
				return mysql.ErrMalformPacket
			}

			paramTypes = data[pos : pos+(paramNum<<1)]
			pos += (paramNum << 1)

			paramValues = data[pos:]
		}

		if err := conn.bindStmtArgs(s, nullBitmaps, paramTypes, paramValues); err != nil {
			return err
		}
	}

	var err error

	switch stmt := s.s.(type) {
	case *sqlparser.Select:
		err = conn.handleSelect(stmt, s.sql, s.args)
	case *sqlparser.Insert:
		err = conn.handleExec(s.s, s.sql, s.args)
	case *sqlparser.Update:
		err = conn.handleExec(s.s, s.sql, s.args)
	case *sqlparser.Delete:
		err = conn.handleExec(s.s, s.sql, s.args)
	case *sqlparser.Replace:
		err = conn.handleExec(s.s, s.sql, s.args)
	default:
		err = fmt.Errorf("command %T not supported now", stmt)
	}

	s.ResetParams()

	return err
}
*/



func (conn *MidConn) makeBindVars(args []interface{}) map[string]interface{} {
	bindVars := make(map[string]interface{}, len(args))

	for i, v := range args {
		bindVars[fmt.Sprintf("v%d", i+1)] = v
	}

	return bindVars
}