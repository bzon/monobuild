# monobuild [work in progress]

*monobuild* or *mb* is a build tool for monorepos.

A monorepo is a single repository that has multiple projects which may be maintained by different teams or people.

In the context of a Go-only monorepo, the following directory structure will make sense.

```txt
cmd
  /server/
  /worker/
  /cli/
pkg
  /api/
  /db/
  /utils/
```

What monorepo tools usually solve is to make builds faster by not building the sources that were not changed.

## CICD problem with monorepo

In Go, it may not be a problem because binaries are built fast. 
The problem is in the context of CICD deployment. 
In CICD, every build, e.g. docker images, may trigger a deployment. 
At scale, this means that if a project has 10 services, these 10 services will be changed. 
If those 10 services has a total of 1000 replicas in production, you will have a massive deployment roll out.

## Existing tools

Every team may have a different way to solve this problem.

### [**Bazel**](https://bazel.build/) and [**Buck**](https://buck.build/)

These are tools made to build multiple languages in a monorepo.

If your team's monorepo is composed of multiple languages, then Bazel or Buck may make sense.

The only problem with using these tools is complexity. You are opting in to something that takes a significant time to learn.

### **Bash scripts**

A hacky and highly customized way to build a monorepo but maintenance cost a highly likely to be big.

### **CI plugins**

This is good if it already solves your problem. The only problem is your build logic highly coupled with the CI provider.

## Goals

monobuild goal is to standardised the build process for a specific language (currently just Go) and replace the use of highly complex bash scripts.

## Definitions

*Targets* - the target binaries to build. In Go, these are usually placed in the `./cmd/` directory.

```yaml
targets:
  - path: ./cmd/server
  - path: ./cmd/worker
  - path: ./cmd/cli-client
```

**Build triggers**

*Target Dependency* - binaries have dependencies and we only want to build these binaries if their dependencies were changed.
In Go, these dependencies are typically Go files in `./pkg/`, `./vendor/` or `./internal/` directory.

```yaml
dep_source_dirs:
  - ./pkg
  - ./vendor
  - ./internal
```

*Watch pattern (glob)* - Changes to these files will trigger a build for a specific target.

Global level.

```yaml
watch_pattern:
  - ./** # will build all the targets if any files were changed in the root directory.
```

Target level.

```yaml
target:
  path: ./cmd/server
  watch_pattern:
  - cmd/worker/** # will build the ./cmd/server/main.go if any files from ./cmd/server/ were changed.
```

## How it works

monobuild only builds a target binary if:

* Any files from the target binary directory has changed.
* Any dependencies that the target binary uses has changed.
* Any of the watched files has changed.

The list of changed files are extracted using `git diff --name-only [provided commit-range]`.

The list of dependencies 

### Go example

Take this example of a **Go** monorepo structure.

```txt
├── bin # outputs
│   ├── server
│   └── worker
├── cmd # targets
│   ├── server
│   │   ├── Dockerfile
│   │   └── Makefile
│   └── worker
│       ├── Dockerfile
│       └── Makefile
├── vendor # packages
│   └── foo 
└── pkg # packages
    └── bar 
|__ monobuild.yaml
```

Example configuration file.

```yaml
dep_source_dirs:
  - pkg
  - internal
  - vendor
targets:
  - path: cmd/server
    watch_pattern:
    - monobuild.yaml
    build_command:
      dir: ./
      command: make
      args:
        - -f
        - cmd/server/Makefile
        - build
  - path: cmd/worker
    watch_pattern:
    - monobuild.yaml
    build_command:
      dir: ./
      command: make
      args:
        - -f
        - cmd/worker/Makefile
        - build
```
