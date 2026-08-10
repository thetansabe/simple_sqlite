package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	// Available if you need it!
	// "github.com/xwb1989/sqlparser"
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
		fmt.Println("Unknown command", command)
		os.Exit(1)
	}
}
