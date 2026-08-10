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
		// Treat anything else as a SQL query, e.g. "SELECT COUNT(*) FROM apples"
		tableName := extractTableName(command)
		count, err := countTableRows(databaseFilePath, tableName)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(count)
	}
}

// extractTableName extracts the table name from "SELECT COUNT(*) FROM <table>".
// Finds the token immediately after the FROM keyword (case-insensitive).
func extractTableName(query string) string {
	fields := strings.Fields(query)
	for i, f := range fields {
		if strings.ToUpper(f) == "FROM" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}
