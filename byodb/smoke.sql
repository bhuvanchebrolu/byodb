-- A end-to-end exercise of every statement the language supports.
-- Run with:  byodb smoke.db < smoke.sql      (PowerShell: Get-Content smoke.sql | .\byodb.exe smoke.db)

create table people (
    country bytes,
    id int64,
    name bytes,
    age int64,
    index (age),
    index (name, age),
    primary key (country, id)
);

insert into people (country, id, name, age) values
    ('de', 1, 'anna', 30),
    ('de', 2, 'bruno', 45),
    ('in', 1, 'chitra', 22),
    ('in', 2, 'deepak', 45),
    ('uk', 1, 'eve', 61),
    ('uk', 2, 'frank', 30);

-- Full scan, in primary key order.
select country, id, name, age from people;

-- Equality on the leading primary key column.
select name from people index by country = 'in';

-- Both key columns as a tuple.
select name from people index by (country, id) = ('uk', 2);

-- A secondary index, and a range over it.
select name, age from people index by age = 45;
select name, age from people index by age > 25 and age < 50;

-- Descending: the bounds are written the other way round.
select name from people index by age < 50 and age > 25;

-- Filter and limit.
select name from people filter age > 40;
select name from people limit 2, 3;
select name from people index by country > 'de' filter age < 50 limit 1, 5;

-- Expressions and aliases.
select name as who, age * 2 as doubled, 'age: ' + name as label from people limit 2;

-- Update, and the index that has to follow it.
update people set age = age + 1 index by country = 'de';
select name, age from people index by country = 'de';
select name from people index by age = 46;

-- Delete.
delete from people filter age > 60;
select country, id, name from people;

-- Errors that should be reported, not ignored.
insert into people (country, id, name, age) values ('de', 1, 'clash', 1);
update people set id = 99 index by country = 'de';
select nosuchcolumn from people;
select name from people where age > 1;

.schema people
.tables
