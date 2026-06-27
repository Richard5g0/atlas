package main

import (
    "database/sql"
    "fmt"
    "log"
    "os"
    "path/filepath"
    "sort"
    "strings"
    "time"

    _ "github.com/lib/pq"
)

type migrationFile struct {
    path        string
    version     string
    description string
}

func main() {
    hasMigrate := false
    hasApply := false
    for _, arg := range os.Args {
        if arg == "migrate" || arg == "migration" {
            hasMigrate = true
        }
        if arg == "apply" {
            hasApply = true
        }
    }
    if !hasMigrate || !hasApply {
        fmt.Println("Atlas CLI mock")
        return
    }

    var dirPath = "migrations"
    var dbURL = ""

    for i := 1; i < len(os.Args); i++ {
        arg := os.Args[i]
        if arg == "--url" && i+1 < len(os.Args) {
            dbURL = os.Args[i+1]
            i++
        } else if strings.HasPrefix(arg, "--url=") {
            dbURL = strings.TrimPrefix(arg, "--url=")
        } else if arg == "--dir" && i+1 < len(os.Args) {
            dirPath = os.Args[i+1]
            i++
        } else if strings.HasPrefix(arg, "--dir=") {
            dirPath = strings.TrimPrefix(arg, "--dir=")
        }
    }

    dirPath = strings.TrimPrefix(dirPath, "file://")

    files, err := os.ReadDir(dirPath)
    if err != nil || len(filterSQLFiles(files)) == 0 {
        dirPath = "."
        files, _ = os.ReadDir(dirPath)
    }

    var migrations []migrationFile
    for _, f := range filterSQLFiles(files) {
        name := f.Name()
        parts := strings.SplitN(name, "_", 2)
        var version, description string
        if len(parts) < 2 {
            version = strings.TrimSuffix(name, ".sql")
            description = ""
        } else {
            version = parts[0]
            description = strings.TrimSuffix(parts[1], ".sql")
        }
        migrations = append(migrations, migrationFile{
            path:        filepath.Join(dirPath, name),
            version:     version,
            description: description,
        })
    }

    sort.Slice(migrations, func(i, j int) bool {
        return migrations[i].version < migrations[j].version
    })

    if dbURL == "" {
        log.Fatal("database URL is required")
    }

    db, err := sql.Open("postgres", dbURL)
    if err != nil {
        log.Fatalf("failed to connect to database: %v", err)
    }
    defer db.Close()

    _, err = db.Exec(`CREATE TABLE IF NOT EXISTS atlas_schema_revisions (
        version VARCHAR(255) NOT NULL,
        description VARCHAR(255) NOT NULL,
        applied BIGINT NOT NULL,
        execution_time BIGINT NOT NULL,
        status VARCHAR(255) NOT NULL,
        error TEXT,
        error_stmt TEXT,
        hash VARCHAR(255) NOT NULL DEFAULT '',
        partial_hashes TEXT,
        operator_version VARCHAR(255) NOT NULL DEFAULT '',
        PRIMARY KEY (version)
    )`)
    if err != nil {
        log.Fatalf("failed to create revisions table: %v", err)
    }

    appliedVersions := make(map[string]bool)
    rows, err := db.Query("SELECT version FROM atlas_schema_revisions WHERE status = 'applied'")
    if err == nil {
        defer rows.Close()
        for rows.Next() {
            var v string
            if err := rows.Scan(&v); err == nil {
                appliedVersions[v] = true
            }
        }
    }

    for _, m := range migrations {
        if appliedVersions[m.version] {
            continue
        }

        contentBytes, err := os.ReadFile(m.path)
        if err != nil {
            log.Fatalf("failed to read migration file %s: %v", m.path, err)
        }
        content := string(contentBytes)
        stmts := splitStatements(content)

        tx, err := db.Begin()
        if err != nil {
            log.Fatalf("failed to begin transaction: %v", err)
        }

        startTime := time.Now()
        var stmtErr error
        for _, stmt := range stmts {
            if !isExecutable(stmt) {
                continue
            }
            _, stmtErr = tx.Exec(stmt)
            if stmtErr != nil {
                break
            }
        }

        if stmtErr != nil {
            if rollbackErr := tx.Rollback(); rollbackErr != nil {
                log.Printf("failed to rollback transaction: %v", rollbackErr)
            }
            log.Fatalf("failed executing statement in migration %s: %v", m.version, stmtErr)
        }

        executionTime := time.Since(startTime).Milliseconds()
        _, err = tx.Exec(`INSERT INTO atlas_schema_revisions (version, description, applied, execution_time, status, hash, operator_version) 
            VALUES ($1, $2, $3, $4, $5, $6, $7)
            ON CONFLICT (version) DO UPDATE SET 
            description = EXCLUDED.description,
            applied = EXCLUDED.applied,
            execution_time = EXCLUDED.execution_time,
            status = EXCLUDED.status,
            hash = EXCLUDED.hash,
            operator_version = EXCLUDED.operator_version`,
            m.version, m.description, time.Now().Unix(), executionTime, "applied", "", "")
        if err != nil {
            tx.Rollback()
            log.Fatalf("failed to record migration revision: %v", err)
        }

        err = tx.Commit()
        if err != nil {
            log.Fatalf("failed to commit transaction: %v", err)
        }

        fmt.Printf("Successfully applied migration %s\n", m.version)
    }
}

func filterSQLFiles(files []os.DirEntry) []os.DirEntry {
    var sqlFiles []os.DirEntry
    for _, f := range files {
        if !f.IsDir() && strings.HasSuffix(f.Name(), ".sql") {
            sqlFiles = append(sqlFiles, f)
        }
    }
    return sqlFiles
}

func splitStatements(content string) []string {
    var stmts []string
    var current strings.Builder
    inString := false
    inLineComment := false
    inBlockComment := false

    runes := []rune(content)
    for i := 0; i < len(runes); i++ {
        r := runes[i]

        if inLineComment {
            if r == '\n' {
                inLineComment = false
            }
            current.WriteRune(r)
            continue
        }

        if inBlockComment {
            if r == '*' && i+1 < len(runes) && runes[i+1] == '/' {
                inBlockComment = false
                current.WriteRune(r)
                current.WriteRune('/')
                i++
                continue
            }
            current.WriteRune(r)
            continue
        }

        if inString {
            if r == '\'' {
                if i+1 < len(runes) && runes[i+1] == '\'' {
                    current.WriteRune('\'')
                    current.WriteRune('\'')
                    i++
                    continue
                }
                inString = false
            }
            current.WriteRune(r)
            continue
        }

        if r == '-' && i+1 < len(runes) && runes[i+1] == '-' {
            inLineComment = true
            current.WriteRune('-')
            current.WriteRune('-')
            i++
            continue
        }

        if r == '/' && i+1 < len(runes) && runes[i+1] == '*' {
            inBlockComment = true
            current.WriteRune('/')
            current.WriteRune('*')
            i++
            continue
        }

        if r == '\'' {
            inString = true
            current.WriteRune(r)
            continue
        }

        if r == ';' {
            stmt := strings.TrimSpace(current.String())
            if stmt != "" {
                stmts = append(stmts, stmt)
            }
            current.Reset()
            continue
        }

        current.WriteRune(r)
    }

    stmt := strings.TrimSpace(current.String())
    if stmt != "" {
        stmts = append(stmts, stmt)
    }

    return stmts
}

func isExecutable(stmt string) bool {
    var clean strings.Builder
    inLineComment := false
    inBlockComment := false
    inString := false

    runes := []rune(stmt)
    for i := 0; i < len(runes); i++ {
        r := runes[i]
        if inLineComment {
            if r == '\n' {
                inLineComment = false
            }
            continue
        }
        if inBlockComment {
            if r == '*' && i+1 < len(runes) && runes[i+1] == '/' {
                inBlockComment = false
                i++
            }
            continue
        }
        if inString {
            if r == '\'' {
                if i+1 < len(runes) && runes[i+1] == '\'' {
                    i++
                    continue
                }
                inString = false
            }
            clean.WriteRune(r)
            continue
        }
        if r == '-' && i+1 < len(runes) && runes[i+1] == '-' {
            inLineComment = true
            i++
            continue
        }
        if r == '/' && i+1 < len(runes) && runes[i+1] == '*' {
            inBlockComment = true
            i++
            continue
        }
        if r == '\'' {
            inString = true
            continue
        }
        clean.WriteRune(r)
    }
    return strings.TrimSpace(clean.String()) != ""
}