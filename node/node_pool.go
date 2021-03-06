/**
 *  author: lim
 *  data  : 18-8-9 下午9:25
 */

package node

import (
	"fmt"

	"time"

	"github.com/lemonwx/log"
	"github.com/lemonwx/xsql/config"
	"github.com/lemonwx/xsql/mysql"
)

const (
	MaxInitFailedSize = 5
	MaxUnUseTime      = time.Hour * 8
)

type Pool struct {
	adminConn *Node
	idleConns chan *Node
	freeConns chan *Node

	maxConnSize uint32

	host     string
	port     int
	user     string
	password string
}

func (p *Pool) NewConn() *Node {
	return NewNode(p.host, p.port, p.user, p.password, "", 0)
}

func (p *Pool) NewAndConnect() (*Node, error) {
	conn := NewNode(p.host, p.port, p.user, p.password, "", 0)
	if err := conn.Connect(); err != nil {
		return nil, err
	}
	conn.lastUseTime = time.Now()
	return conn, nil
}

func NewNodePool(initSize, idleSize, maxConnSize uint32, cfg *config.Node) (*Pool, error) {
	if initSize > idleSize {
		return nil, fmt.Errorf("pool's init size must < idle size")
	}
	if idleSize > maxConnSize {
		return nil, fmt.Errorf("pool's idle size must < max size")
	}

	p := &Pool{
		maxConnSize: maxConnSize,
		host:        cfg.Host,
		port:        cfg.Port,
		user:        cfg.User,
		password:    cfg.Password,
	}

	if conn, err := p.NewAndConnect(); err != nil {
		return nil, err
	} else {
		p.adminConn = conn
	}

	failedSize := 0
	p.idleConns = make(chan *Node, idleSize)
	p.freeConns = make(chan *Node, maxConnSize-idleSize)

	count := uint32(0)
	for count < initSize {
		if conn, err := p.NewAndConnect(); err != nil {
			failedSize++
			if failedSize > MaxInitFailedSize {
				return nil, fmt.Errorf("too many errors when connect to backend")
			}
		} else {
			p.idleConns <- conn
			count++
			failedSize = 0
		}
	}

	for count < maxConnSize {
		if count < idleSize {
			p.idleConns <- p.NewConn()
		} else {
			p.freeConns <- p.NewConn()
		}
		count++
	}
	return p, nil
}

func (p *Pool) tryReuse(conn *Node, schema string) (*Node, error) {
	if time.Now().Sub(conn.lastUseTime) > MaxUnUseTime {
		log.Debugf("this conn not used more than %v", MaxUnUseTime)
		if err := conn.Ping(); err != nil {
			log.Debugf("execute ping use this conn failed, close it")
			conn.Close()
			conn.conn = nil
		}
	}

	if conn.conn != nil {
		if err := p.useDB(conn, schema); err != nil {
			conn.Close()
			p.PutConn(conn)
			return nil, err
		}
		return conn, nil
	}

	if err := conn.Connect(); err != nil {
		conn.Close()
		p.PutConn(conn)
		return nil, err
	}

	if err := p.useDB(conn, schema); err != nil {
		conn.Close()
		p.PutConn(conn)
		return nil, err
	}
	return conn, nil
}

func (p *Pool) GetConnFromIdle(schema string) (*Node, error) {
	var conn *Node
	select {
	case conn = <-p.idleConns:
		return p.tryReuse(conn, schema)
	default:
		return nil, fmt.Errorf("idle list empty, try get from free list")
	}
}

func (p *Pool) GetConn(schema string) (*Node, error) {
	// first try get conn from idle
	if conn, err := p.GetConnFromIdle(schema); err == nil {
		return conn, nil
	}

	// during this time,
	//      1. may some conn put back to idle list, we expect all conn get from idle list
	//   or 2. may free empty and idle not empty
	// so try get conn from both idle and free
	var conn *Node
	select {
	case conn = <-p.idleConns:
	case conn = <-p.freeConns:
	}
	return p.tryReuse(conn, schema)
}

func (p *Pool) useDB(back *Node, schema string) error {
	if len(schema) == 0 || back.Db == schema {
		return nil
	}

	if _, err := back.Execute(mysql.COM_INIT_DB, []byte(schema)); err != nil {
		return err
	}
	back.Db = schema

	return nil
}

func (p *Pool) freeConn(node *Node) {
	node.Close()
	select {
	case p.freeConns <- node:
		return
	default:
		log.Errorf("unexpected both full of idle and free node list")
		return
	}
}

func (p *Pool) PutConn(node *Node) {
	node.lastUseTime = time.Now()
	select {
	case p.idleConns <- node:
		return
	default:
		p.freeConn(node)
	}
}

func (p *Pool) String() string {
	return fmt.Sprintf("%s:%d", p.host, p.port)
}

func (p *Pool) DumpInfo() {
	log.Errorf("pool:%v idle: %d, free: %d", p, len(p.idleConns), len(p.freeConns))
}
