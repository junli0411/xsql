/**
 *  author: lim
 *  data  : 18-8-28 下午10:43
 */

package server

import (
	"strconv"
	"time"
)

type field interface {
	avg() int64
	add(int64)
	plus(ff field)
	fmt() []byte
	clear()
}

type timeField struct {
	t time.Duration
	c int64
}

func (f *timeField) avg() int64 {
	if f.c != 0 {
		return int64(f.t) / f.c
	}
	return 0
}

func (f *timeField) add(t int64) {
	f.c += 1
	f.t += time.Duration(t)
}

func (f *timeField) plus(ff field) {
	field := ff.(*timeField)
	f.c += field.c
	f.t += field.t
}

func (f *timeField) fmt() []byte {
	ret := []byte{}
	t := time.Duration(f.t).String()
	c := strconv.FormatInt(f.c, 10)
	avg := time.Duration(f.avg()).String()

	ret = append(ret, byte(len(t)))
	ret = append(ret, t...)

	ret = append(ret, byte(len(c)))
	ret = append(ret, c...)

	ret = append(ret, byte(len(avg)))
	ret = append(ret, avg...)

	return ret
}

func (f *timeField) clear() {
	f.c = 0
	f.t = 0
}

type countField struct {
	count int64
	c     int64
}

func (f *countField) add(count int64) {
	f.count += count
	f.c += 1
}

func (f *countField) avg() int64 {
	if f.c == 0 {
		return 0
	}
	return f.count / f.c
}

func (f *countField) plus(ff field) {
	field := ff.(*countField)
	f.c += field.c
	f.count += field.count
}

func (f *countField) clear() {
	f.c = 0
	f.count = 0
}

func (f *countField) fmt() []byte {
	ret := []byte{}
	count := strconv.FormatInt(f.count, 10)
	c := strconv.FormatInt(f.c, 10)
	avg := strconv.FormatInt(f.avg(), 10)

	ret = append(ret, byte(len(count)))
	ret = append(ret, count...)

	ret = append(ret, byte(len(c)))
	ret = append(ret, c...)

	ret = append(ret, byte(len(avg)))
	ret = append(ret, avg...)
	return ret
}

type Stat struct {
	SqlparseT         *timeField
	RouteT            *timeField
	VersionT          *timeField
	ExecT             *timeField
	ChkInuseT         *timeField
	ClearT            *timeField
	GetConn           *timeField
	PutConn           *timeField
	Dispatch          *timeField
	VWaitBatchT       *timeField
	VWaitRespT        *timeField
	BatchReqCount     *countField
	FullReqCount      *countField
	TickerReqCount    *countField
	BlockRequestCount *countField
	SendT             *timeField
}

func newStat() *Stat {
	return &Stat{
		SqlparseT:         &timeField{},
		RouteT:            &timeField{},
		VersionT:          &timeField{},
		ExecT:             &timeField{},
		ChkInuseT:         &timeField{},
		ClearT:            &timeField{},
		GetConn:           &timeField{},
		PutConn:           &timeField{},
		Dispatch:          &timeField{},
		VWaitBatchT:       &timeField{},
		VWaitRespT:        &timeField{},
		BatchReqCount:     &countField{},
		FullReqCount:      &countField{},
		TickerReqCount:    &countField{},
		BlockRequestCount: &countField{},
		SendT:             &timeField{},
	}
}

func (s *Stat) getTheoryAvg() time.Duration {
	max := int64(0)
	vt := s.VersionT.avg()
	et := s.ExecT.avg()
	if vt > et {
		max = vt
	} else {
		max = et
	}

	return time.Duration(s.SqlparseT.avg() + s.RouteT.avg() + max + s.ChkInuseT.avg() + s.ClearT.avg())
}

func (s *Stat) clear() {
	s.SqlparseT.clear()
	s.RouteT.clear()
	s.VersionT.clear()
	s.ExecT.clear()
	s.ChkInuseT.clear()
	s.ClearT.clear()
	s.GetConn.clear()
	s.PutConn.clear()
	s.Dispatch.clear()
	s.VWaitBatchT.clear()
	s.VWaitRespT.clear()
	s.BatchReqCount.clear()
	s.FullReqCount.clear()
	s.TickerReqCount.clear()
}
