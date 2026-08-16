#!/usr/bin/env python3
"""Compare payments_api Go fixture DDL against Django model definitions.

Reads the Go fixture DDL from internal/testutil/db.go, extracts column
definitions, then compares against Django's expected columns for the
same tables. Reports drift and exits non-zero when columns are missing
from the Go fixture (meaning the fixture is behind Django migrations).

Usage:
    python scripts/fixture_check.py [--micro-flash-api PATH]

The default micro_flash_api path is ../microtik_flash_api relative to the
payments_api directory.
"""
import os
import re
import sys


GO_FIXTURE = os.path.join(os.path.dirname(__file__), '..', 'internal', 'testutil', 'db.go')
DJANGO_MODELS = os.path.join(os.path.dirname(__file__), '..', '..', 'microtik_flash_api')


def extract_go_columns(fixture_path: str) -> dict[str, list[str]]:
    """Parse CREATE TABLE blocks from Go fixture DDL and return {table: [columns]}."""
    with open(fixture_path) as f:
        content = f.read()

    tables: dict[str, list[str]] = {}
    # Match: CREATE TABLE IF NOT EXISTS <table> ( ... );
    for m in re.finditer(
        r'CREATE\s+TABLE\s+IF\s+NOT\s+EXISTS\s+(\w+)\s*\((.*?)\);',
        content, re.S | re.IGNORECASE,
    ):
        table_name = m.group(1)
        body = m.group(2)
        columns = []
        for line in body.split('\n'):
            line = line.strip().rstrip(',')
            if not line or line.startswith(('PRIMARY', 'UNIQUE', 'KEY', 'INDEX', 'CONSTRAINT')):
                continue
            col_match = re.match(r'`?(\w+)`?\s+', line)
            if col_match:
                columns.append(col_match.group(1))
        tables[table_name] = columns

    return tables


def extract_django_columns(django_path: str) -> dict[str, list[str]]:
    """Read Django models.py for payments app and extract expected DB columns.

    Falls back to parsing models.py directly when Django can't initialize
    (no DB, missing settings, etc.), which is the common case for this
    standalone check script.
    """
    models_path = os.path.join(django_path, 'payments', 'models.py')
    if not os.path.exists(models_path):
        print(f"WARN: models.py not found at {models_path}")
        return {}

    with open(models_path) as f:
        content = f.read()

    tables: dict[str, list[str]] = {}

    # Find class definitions with db_table
    for m in re.finditer(
        r"class\s+(\w+)\(.*?\):(.*?)(?=\nclass\s|\Z)",
        content, re.S,
    ):
        class_body = m.group(2)
        db_table_m = re.search(r"db_table\s*=\s*['\"](\w+)['\"]", class_body)
        if not db_table_m:
            continue
        db_table = db_table_m.group(1)

        columns = ['id']  # always present
        # Find field definitions: name = models.FieldType(...)
        for fm in re.finditer(r"^\s+(\w+)\s*=\s*models\.(\w+)\(", class_body, re.M):
            field_name = fm.group(1)
            field_type = fm.group(2)
            if field_name in ('Meta', '__str__'):
                continue
            # ForeignKey creates <name>_id column
            if field_type == 'ForeignKey':
                columns.append(f'{field_name}_id')
            else:
                columns.append(field_name)

        tables[db_table] = columns

    return tables


def main():
    fixture_path = os.path.abspath(GO_FIXTURE)
    django_path = os.path.abspath(DJANGO_MODELS)

    if len(sys.argv) > 2 and sys.argv[1] == '--micro-flash-api':
        django_path = os.path.abspath(sys.argv[2])

    if not os.path.exists(fixture_path):
        print(f"FAIL: Go fixture not found at {fixture_path}")
        return 1

    go_tables = extract_go_columns(fixture_path)
    django_tables = extract_django_columns(django_path)

    # Only check tables that exist in both
    common = set(go_tables.keys()) & set(django_tables.keys())
    if not common:
        print("WARN: No common tables found between Go fixture and Django models")
        return 0

    failures = 0
    warnings = 0

    for table in sorted(common):
        go_cols = set(c.lower() for c in go_tables[table])
        django_cols = set(c.lower() for c in django_tables[table])

        missing_in_go = django_cols - go_cols
        extra_in_go = go_cols - django_cols

        if missing_in_go:
            print(f"FAIL {table}: missing in Go fixture: {sorted(missing_in_go)}")
            failures += 1
        elif extra_in_go:
            print(f"WARN {table}: extra in Go fixture (not in Django): {sorted(extra_in_go)}")
            warnings += 1
        else:
            print(f"OK {table}: columns match ({len(go_cols)} cols)")

    print(f"\n[fixture_check] tables_checked={len(common)} failures={failures} warnings={warnings}")

    if failures:
        print("FAIL: Go fixture DDL is behind Django migrations")
        return 1
    print("OK: Go fixture DDL is in sync with Django migrations")
    return 0


if __name__ == '__main__':
    sys.exit(main())
