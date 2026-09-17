# AgentMux Project and Collection Model

## 1. Why this model exists

AgentMux must not assume:

```text
D:\AI\Projects\<first-level-folder>
```

always means one development project.

Users may organize work like:

```text
D:\AI\Projects
└─ 2026 AgentMux
   ├─ Reference
   ├─ Design
   ├─ Notes
   ├─ Archive
   └─ AgentMux
      ├─ .git
      ├─ CLAUDE.md
      └─ source...
```

The outer folder is a human organization layer.

The inner `AgentMux` directory is the real development project.

## 2. Terminology

### Projects Root

The broad user-controlled root containing groups and/or projects.

Default Windows value:

```text
D:\AI\Projects
```

### Collection / Group

An organizational folder.

Example:

```text
D:\AI\Projects\2026 AgentMux
```

A collection may contain:

- reference documents;
- screenshots;
- plans;
- archives;
- notes;
- one or more development projects.

A collection is not automatically a Git repository or Claude working directory.

### Project

A registered development directory managed by AgentMux.

Example:

```text
D:\AI\Projects\2026 AgentMux\AgentMux
```

A Project may contain:

- `.git`;
- `CLAUDE.md`;
- `.claude`;
- source code;
- package/build files;
- project documentation.

AgentMux persistent sessions attach to Projects, not Collections.

## 3. Project registration

The most reliable workflow is explicit registration.

The user chooses a directory once.

AgentMux stores:

```text
projectId
displayName
hostPath
runtimePath
collectionPath (optional)
```

After registration, the stored project path is authoritative.

AgentMux should not repeatedly infer the project path from folder names.

## 4. Automatic discovery

AgentMux may suggest candidate projects by scanning within configured roots.

Possible indicators include:

```text
.git/
CLAUDE.md
package.json
go.mod
pyproject.toml
Cargo.toml
*.sln
*.csproj
pom.xml
build.gradle
```

Discovery is advisory.

Do not automatically register every matching directory.

Do not treat folders such as:

```text
Reference
Archive
Notes
Design
Documents
Screenshots
```

as projects merely because they contain files.

## 5. Scan depth

Do not assume fixed depth 1.

Support a configurable discovery depth, such as 2 or 3.

For example:

```text
D:\AI\Projects
  depth 1: 2026 AgentMux
  depth 2: AgentMux ← project
```

Avoid unbounded recursive scanning.

Ignore common heavy/generated directories:

```text
node_modules
.git
dist
build
target
vendor
.venv
venv
.cache
```

## 6. New Project workflow

New Project should allow:

```text
Collection:
D:\AI\Projects\2026 AgentMux

Project Name:
AgentMux

Final Path:
D:\AI\Projects\2026 AgentMux\AgentMux
```

The user may also choose:

```text
Create project directly under Projects Root
```

The system must not require a collection folder.

## 7. Git boundary

Git is initialized only inside the selected Project path.

For AgentMux:

```text
D:\AI\Projects\2026 AgentMux\AgentMux\.git
```

Do not initialize Git in:

```text
D:\AI\Projects\2026 AgentMux
```

unless the user explicitly requests that broader folder to be the repository.

## 8. Claude boundary

Claude Code is started with the Project path as current working directory.

For AgentMux:

```text
cd "D:\AI\Projects\2026 AgentMux\AgentMux"
claude
```

or WSL:

```text
cd "/mnt/d/AI/Projects/2026 AgentMux/AgentMux"
claude
```

Do not start Claude from the collection folder by default.

## 9. Parent CLAUDE.md rule

Because parent `CLAUDE.md` files may be inherited by Claude Code, users should avoid placing a `CLAUDE.md` in collection folders unless they intentionally want those rules shared by projects below.

Recommended:

```text
2026 AgentMux\
├─ Reference\
├─ Notes\
└─ AgentMux\
   └─ CLAUDE.md
```

Avoid by default:

```text
2026 AgentMux\
├─ CLAUDE.md
└─ AgentMux\
```

## 10. Runtime metadata boundary

AgentMux runtime state must not be written into the project repository.

Do not create project-local runtime folders such as:

```text
.ai-workstation/
.agentmux-runtime/
```

unless a later feature explicitly requires an opt-in project file.

Use AgentMux's own data directory/database instead.
