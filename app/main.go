package main

import (
	"fmt"
	"log"
	"os"
	"strings"
)

// Usage: your_program.sh sample.db .dbinfo
func main() {
	databaseFilePath := os.Args[1]
	command := os.Args[2]

	switch command {
	case ".dbinfo":
		info, err := readDbInfo(databaseFilePath)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("database page size: %v\n", info.pageSize)
		fmt.Printf("number of tables: %v\n", info.tableCount)

	case ".tables":
		names, err := readTableNames(databaseFilePath)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(strings.Join(names, " "))

	default:
		// Treat anything else as a SQL SELECT query.
		q, err := parseSelect(command)
		if err != nil {
			log.Fatal(err)
		}

		if q.isCount {
			// SELECT COUNT(*) FROM <table>
			count, err := countTableRows(databaseFilePath, q.table)
			if err != nil {
				log.Fatal(err)
			}
			fmt.Println(count)
		} else {
			// SELECT col1, col2, ... FROM <table>
			if err := executeSelect(databaseFilePath, q); err != nil {
				log.Fatal(err)
			}
		}
	}
}
