package main

import (
	"fmt"
	"os"

	"byodb/ql"
	"byodb/relational"
)

func main() {
	os.Remove("test_verify.db")
	defer os.Remove("test_verify.db")

	db, err := relational.Open("test_verify.db")
	if err != nil {
		fmt.Printf("❌ Failed to open db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	queries := []string{
		// DDL
		`CREATE TABLE departments (
			id int64 AUTO_INCREMENT,
			code bytes UNIQUE,
			dept_name bytes,
			PRIMARY KEY (id)
		);`,

		`CREATE TABLE employees (
			id int64 AUTO_INCREMENT,
			dept_id int64,
			email bytes UNIQUE,
			name bytes,
			salary float64,
			status bytes DEFAULT 'active',
			age int64 CHECK (age >= 18),
			PRIMARY KEY (id),
			FOREIGN KEY (dept_id) REFERENCES departments (id) ON DELETE CASCADE,
			INDEX (salary)
		);`,

		`CREATE TABLE IF NOT EXISTS projects (
			id int64 AUTO_INCREMENT,
			title bytes,
			PRIMARY KEY (id)
		);`,

		`DROP TABLE IF EXISTS old_logs;`,

		`ALTER TABLE employees ADD location bytes;`,
		`ALTER TABLE employees ADD COLUMN department_id int64;`,

		// DML
		`INSERT INTO departments (code, dept_name) VALUES ('ENG', 'Engineering'), ('MKT', 'Marketing');`,

		`INSERT INTO employees (id, dept_id, name, salary, age) VALUES (1, 1, 'Alice Smith', 75000.0, 28);`,

		`INSERT INTO employees (dept_id, name, salary, age) VALUES 
			(1, 'Bob Jones', 48000.0, 35),
			(2, 'Charlie Brown', 92000.0, 42),
			(2, 'David Miller', 55000.0, 22);`,

		`REPLACE INTO employees (id, name, salary, status, age) VALUES (1, 'Alice Smith-Doe', 80000.0, 'active', 29);`,

		`UPDATE employees SET salary = salary * 1.10 WHERE status = 'active';`,
		`UPDATE employees SET status = 'promoted' INDEX BY id = 3;`,

		`DELETE FROM employees WHERE age < 18;`,

		// DQL
		`SELECT * FROM employees;`,
		`SELECT id, name AS employee_name, salary * 12.0 AS annual_salary FROM employees;`,
		`SELECT DISTINCT status FROM employees;`,
		`SELECT * FROM employees INDEX BY salary > 50000.0 AND salary < 100000.0;`,
		`SELECT id, name, salary FROM employees WHERE status = 'active' AND age >= 25;`,

		// Aggregations
		`SELECT COUNT(*), AVG(salary), MAX(age) FROM employees;`,
		`SELECT status, COUNT(*), SUM(salary) FROM employees GROUP BY status;`,

		// Joins
		`SELECT employees.name, departments.dept_name FROM employees JOIN departments ON employees.dept_id = departments.id;`,
		`SELECT employees.name, departments.dept_name FROM employees LEFT JOIN departments ON employees.dept_id = departments.id;`,

		// Order By & Pagination
		`SELECT id, name, salary FROM employees ORDER BY salary DESC, age ASC;`,
		`SELECT id, name FROM employees ORDER BY id ASC LIMIT 10 OFFSET 0;`,
		`SELECT id, name FROM employees ORDER BY id ASC OFFSET 0 LIMIT 10;`,
		`SELECT id, name FROM employees LIMIT 0, 10;`,

		// Operators & Expressions
		`SELECT * FROM employees WHERE (salary >= 50000.0 AND status = 'active') OR NOT (age < 30);`,
		`SELECT * FROM employees WHERE status IS NOT NULL;`,
		`SELECT * FROM employees WHERE name LIKE 'A%';`,
		`SELECT * FROM employees WHERE name LIKE '%Smith%';`,
		`SELECT * FROM employees WHERE name LIKE 'B_b%';`,
		`SELECT * FROM employees WHERE salary BETWEEN 50000.0 AND 90000.0;`,
		`SELECT * FROM employees WHERE status IN ('active', 'on_leave');`,
		`SELECT * FROM employees WHERE id NOT IN (1, 2, 5);`,
		`SELECT * FROM employees WHERE id IN (SELECT id FROM employees WHERE salary > 70000.0);`,

		// Set Operations
		`SELECT id FROM employees WHERE salary > 80000.0 UNION SELECT id FROM employees WHERE age < 25;`,
		`SELECT id FROM employees WHERE salary > 80000.0 UNION ALL SELECT id FROM employees WHERE age < 25;`,
		`SELECT id FROM employees WHERE salary > 60000.0 INTERSECT SELECT id FROM employees WHERE age < 30;`,

		// Views
		`CREATE VIEW high_salary_employees AS SELECT id, name, salary, status FROM employees WHERE salary >= 75000.0;`,
		`SELECT name, salary FROM high_salary_employees ORDER BY salary DESC;`,
		`DROP VIEW high_salary_employees;`,
	}

	for i, q := range queries {
		err := db.Update(func(tx *relational.DBTX) error {
			res, err := ql.Run(tx, q)
			if err != nil {
				return err
			}
			if res.Rows != nil {
				var count int
				for res.Rows.Valid() {
					count++
					res.Rows.Next()
				}
			}
			return nil
		})
		if err != nil {
			fmt.Printf("❌ Error on query #%d:\nQuery: %s\nError: %v\n\n", i+1, q, err)
			os.Exit(1)
		} else {
			fmt.Printf("✅ Query #%d OK\n", i+1)
		}
	}

	fmt.Println("\n🎉 ALL QUERIES PASSED VERIFICATION PERFECTLY!")
}
