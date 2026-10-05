# BYODB - Allowed SQL Queries & Syntax Reference

This document provides a comprehensive guide to all supported SQL statements, syntax rules, operators, and expressions implemented in the BYODB engine.

---

## 📋 Table of Contents
1. [Data Definition Language (DDL)](#1-data-definition-language-ddl)
2. [Data Manipulation Language (DML)](#2-data-manipulation-language-dml)
3. [Data Query Language (DQL)](#3-data-query-language-dql)
4. [Operators & Expressions](#4-operators--expressions)
5. [Set Operations](#5-set-operations)
6. [Virtual Views](#6-virtual-views)
7. [Transaction Control](#7-transaction-control)

---

## 1. Data Definition Language (DDL)

### Supported Column Types
- `int64` (or `int`, `integer`, `bigint`) - 64-bit signed integer
- `float64` (or `float`, `double`, `real`) - IEEE 754 64-bit floating point number
- `bytes` (or `string`, `text`, `blob`) - Arbitrary byte string / text

---

### `CREATE TABLE`
Defines a new table schema with primary keys, optional secondary indexes, default column values, check constraints, auto-increment sequences, unique constraints, and foreign key references.

```sql
-- Parent Table with AUTO_INCREMENT and UNIQUE constraints
CREATE TABLE departments (
    id int64 AUTO_INCREMENT,
    code bytes UNIQUE,
    dept_name bytes,
    PRIMARY KEY (id)
);

-- Child Table with FOREIGN KEY & ON DELETE CASCADE
CREATE TABLE employees (
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
);

-- Conditional Creation
CREATE TABLE IF NOT EXISTS projects (
    id int64 AUTO_INCREMENT,
    title bytes,
    PRIMARY KEY (id)
);
```

---

### `DROP TABLE`
Removes a table definition and all row/index data stored within it.

```sql
DROP TABLE employees;
DROP TABLE IF EXISTS old_logs;
```

---

### `ALTER TABLE`
Adds a new column to an existing table schema (defaults existing rows to `NULL`).

```sql
ALTER TABLE employees ADD location bytes;
ALTER TABLE employees ADD COLUMN department_id int64;
```

---

## 2. Data Manipulation Language (DML)

### `INSERT INTO`
Adds one or more rows to a table. Unsupplied non-primary-key columns populate with their `DEFAULT` value or `NULL`.

```sql
-- Single row insert
INSERT INTO employees (id, name, salary, age) VALUES (1, 'Alice Smith', 75000.0, 28);

-- Multi-row insert with default values
INSERT INTO employees (id, name, salary, age) VALUES 
    (2, 'Bob Jones', 48000.0, 35),
    (3, 'Charlie Brown', 92000.0, 42),
    (4, 'David Miller', 55000.0, 22);
```

---

### `REPLACE INTO` (Upsert)
Inserts a new row or updates an existing row if a primary key collision occurs.

```sql
REPLACE INTO employees (id, name, salary, status, age) VALUES (1, 'Alice Smith-Doe', 80000.0, 'active', 29);
```

---

### `UPDATE`
Assigns expressions to columns of rows matching optional index ranges or filters.

```sql
-- Update with WHERE filter
UPDATE employees SET salary = salary * 1.10 WHERE status = 'active';

-- Update with explicit B-Tree index scan
UPDATE employees SET status = 'promoted' INDEX BY id = 3;
```

---

### `DELETE FROM`
Removes matching rows from a table.

```sql
-- Delete with WHERE filter
DELETE FROM employees WHERE age < 18;

-- Delete with explicit B-Tree index scan
DELETE FROM employees INDEX BY id = 4;
```

---

## 3. Data Query Language (DQL)

### `SELECT`
Retrieves rows from tables or views using optional filtering, grouping, sorting, deduplication, and pagination.

```sql
-- Wildcard Selection
SELECT * FROM employees;
SELECT e.* FROM employees AS e;

-- Column Selection & Aliases
SELECT id, name AS employee_name, salary * 12.0 AS annual_salary FROM employees;

-- Deduplication (DISTINCT)
SELECT DISTINCT status FROM employees;

-- Index B-Tree Bounds (INDEX BY)
SELECT * FROM employees INDEX BY salary > 50000.0 AND salary < 100000.0;

-- WHERE / FILTER Predicate
SELECT id, name, salary FROM employees WHERE status = 'active' AND age >= 25;
```

---

### Aggregations & `GROUP BY`
Supported aggregate functions: `COUNT(*)`, `COUNT(col)`, `SUM(col)`, `AVG(col)`, `MIN(col)`, `MAX(col)`.

```sql
-- Global Aggregation
SELECT COUNT(*), AVG(salary), MAX(age) FROM employees;

-- Grouped Aggregation
SELECT status, COUNT(*), SUM(salary) FROM employees GROUP BY status;
```

---

### Table Joins (`JOIN`)
Supports `INNER JOIN`, `LEFT JOIN` (Left Outer), and `RIGHT JOIN` (Right Outer).

```sql
-- Inner Join
SELECT employees.name, departments.dept_name 
FROM employees 
JOIN departments ON employees.id = departments.id;

-- Left Outer Join
SELECT employees.name, departments.dept_name 
FROM employees 
LEFT JOIN departments ON employees.id = departments.id;
```

---

### `ORDER BY`
Sorts output rows by one or more expressions ascending (`ASC`) or descending (`DESC`).

```sql
SELECT id, name, salary FROM employees ORDER BY salary DESC, age ASC;
```

---

### Pagination (`LIMIT` & `OFFSET`)
Limits the maximum returned rows and skips an offset.

```sql
-- Standard Limit / Offset
SELECT id, name FROM employees ORDER BY id ASC LIMIT 10 OFFSET 20;

-- Alternative Offset / Limit Syntax
SELECT id, name FROM employees ORDER BY id ASC OFFSET 20 LIMIT 10;

-- Comma Syntax: LIMIT offset, count
SELECT id, name FROM employees LIMIT 20, 10;
```

---

## 4. Operators & Expressions

### Comparison & Boolean Operators
- Comparison: `=`, `!=`, `<`, `<=`, `>`, `>=`
- Boolean: `AND`, `OR`, `NOT`
- Null Checks: `IS NULL`, `IS NOT NULL`

```sql
SELECT * FROM employees WHERE (salary >= 50000.0 AND status = 'active') OR NOT (age < 30);
SELECT * FROM employees WHERE status IS NOT NULL;
```

---

### Pattern Matching (`LIKE`)
Supports wildcard `%` (matches any sequence of characters) and `_` (matches any single character).

```sql
SELECT * FROM employees WHERE name LIKE 'A%';       -- Starts with 'A'
SELECT * FROM employees WHERE name LIKE '%Smith%';  -- Contains 'Smith'
SELECT * FROM employees WHERE name LIKE 'B_b%';     -- Single char wildcard ('Bob', 'Bib')
```

---

### Range Checks (`BETWEEN`)

```sql
SELECT * FROM employees WHERE salary BETWEEN 50000.0 AND 90000.0;
```

---

### List & Subquery Membership (`IN` / `NOT IN`)

```sql
-- List Membership
SELECT * FROM employees WHERE status IN ('active', 'on_leave');
SELECT * FROM employees WHERE id NOT IN (1, 2, 5);

-- Subquery Membership
SELECT * FROM employees WHERE id IN (SELECT id FROM employees WHERE salary > 70000.0);
```

---

## 5. Set Operations

Combine result sets of two queries using `UNION`, `UNION ALL`, or `INTERSECT`.

```sql
-- UNION (Deduplicated combination)
SELECT id FROM employees WHERE salary > 80000.0 
UNION 
SELECT id FROM employees WHERE age < 25;

-- UNION ALL (Preserves duplicates)
SELECT id FROM employees WHERE salary > 80000.0 
UNION ALL 
SELECT id FROM employees WHERE age < 25;

-- INTERSECT (Common rows)
SELECT id FROM employees WHERE salary > 60000.0 
INTERSECT 
SELECT id FROM employees WHERE age < 30;
```

---

## 6. Virtual Views

Create and drop virtual views representing pre-defined query execution logic.

```sql
-- Create View
CREATE VIEW high_salary_employees AS 
SELECT id, name, salary, status FROM employees WHERE salary >= 75000.0;

-- Query View
SELECT name, salary FROM high_salary_employees ORDER BY salary DESC;

-- Drop View
DROP VIEW high_salary_employees;
```

---

## 7. Transaction Control

BYODB supports full ACID transaction semantics with snapshot isolation.

```sql
BEGIN;             -- Starts a transaction
INSERT INTO employees (id, name, salary, age) VALUES (10, 'Grace', 65000.0, 30);
UPDATE employees SET salary = 68000.0 WHERE id = 10;
COMMIT;            -- Commits all transaction changes atomically

-- Rollback Example
BEGIN;
DELETE FROM employees WHERE age > 60;
ROLLBACK;          -- Aborts transaction and restores previous state
```
