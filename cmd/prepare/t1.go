/**
 *  author: lim
 *  data  : 18-4-24 下午9:52
 */

package main

import (
	"database/sql"
	"fmt"
	_ "github.com/go-sql-driver/mysql"
	"time"
)

func handleErr(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	//db, err := sql.Open("mysql", "root:root@tcp(172.17.0.3:5518)/db")
	db, err := sql.Open("mysql", "root:root@tcp(192.168.1.7:1234)/db")
	handleErr(err)

	stmt, err := db.Prepare("insert into tb(id, name) values (?, ?)")
	//stmt, err := db.Prepare("select id, name from tb where id = ?")
	handleErr(err)
	fmt.Println(stmt)

	af, err := stmt.Exec(100, 11)
	handleErr(err)

	fmt.Println(af.LastInsertId())

	/*
		var id int
		var name string
		var v uint64
		fmt.Println("---------------")
		for rs.Next() {
			err = rs.Scan(&v, &id, &name)
			fmt.Println(err)
			fmt.Println("----", id, name)
		}
	*/

	for {
		time.Sleep(time.Second * 10)
	}

}
