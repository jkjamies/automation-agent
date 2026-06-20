package fixflow

import (
	"os"
	"sort"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

type readFileArgs struct {
	Path string `json:"path"`
}
type readFileResult struct {
	Content string `json:"content"`
}
type listDirArgs struct {
	Path string `json:"path"`
}
type listDirResult struct {
	Entries []string `json:"entries"`
}

// repoTools returns read-only tools (read_file, list_dir) rooted at the checkout, so
// a tool-using agent can examine the real repository — its standards docs, existing
// tests, and layout — and ground decisions in what the repo actually does.
func repoTools(root string) ([]tool.Tool, error) {
	readFile, err := functiontool.New(functiontool.Config{
		Name:        "read_file",
		Description: "Read a repository file by its repo-relative path (e.g. \"src/main.go\" or \"AGENTS.md\").",
	}, func(_ tool.Context, args readFileArgs) (readFileResult, error) {
		c, err := ReadFile(root, args.Path)
		if err != nil {
			return readFileResult{}, err
		}
		return readFileResult{Content: c}, nil
	})
	if err != nil {
		return nil, err
	}

	listDir, err := functiontool.New(functiontool.Config{
		Name:        "list_dir",
		Description: "List the files and subdirectories of a repository directory by its repo-relative path. Use \".\" for the repository root.",
	}, func(_ tool.Context, args listDirArgs) (listDirResult, error) {
		entries, err := listDirEntries(root, args.Path)
		if err != nil {
			return listDirResult{}, err
		}
		return listDirResult{Entries: entries}, nil
	})
	if err != nil {
		return nil, err
	}

	return []tool.Tool{readFile, listDir}, nil
}

// listDirEntries lists a checkout directory (path-safe), suffixing subdirectories
// with "/" and hiding the .git directory.
func listDirEntries(root, rel string) ([]string, error) {
	full, err := safeJoin(root, rel)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.Name() == ".git" {
			continue
		}
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
