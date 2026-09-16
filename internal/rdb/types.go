package rdb

import (
	"fmt"
	"path"
	"strings"
	"time"

	"altrdb-telegram-bot/internal/model"
)

// Wire types for the ALT RDB (ALTRepo) API.
//
// They are deliberately hand-written instead of generated: only four endpoints
// are used, the upstream document is large, and a hand-written struct makes it
// obvious which fields the bot actually depends on. Unknown fields are ignored
// by encoding/json, and a JSON null decodes into the zero value, so an API that
// grows or starts returning nulls will not break us.

// BranchesResponse is GET /task/progress/all_tasks_branches.
type BranchesResponse struct {
	Length   int      `json:"length"`
	Branches []string `json:"branches"`
}

// TasksListResponse is GET /task/progress/last_tasks and /find_tasks.
type TasksListResponse struct {
	RequestArgs map[string]any `json:"request_args"`
	Length      int            `json:"length"`
	Tasks       []TaskElement  `json:"tasks"`
}

// TaskElement is one entry of the live task feed.
type TaskElement struct {
	TaskID       int64       `json:"task_id"`
	TaskRepo     string      `json:"task_repo"`
	TaskState    string      `json:"task_state"`
	TaskOwner    string      `json:"task_owner"`
	TaskTry      int         `json:"task_try"`
	TaskIter     int         `json:"task_iter"`
	TaskTestonly int         `json:"task_testonly"`
	TaskChanged  string      `json:"task_changed"`
	TaskMessage  string      `json:"task_message"`
	TaskStage    string      `json:"task_stage"`
	Dependencies []int64     `json:"dependencies"`
	Subtasks     []Subtask   `json:"subtasks"`
	Approval     []Approval  `json:"approval"`
	Iterations   []Iteration `json:"iterations"`
}

// Subtask is one build/delete/copy step of a task.
type Subtask struct {
	SubtaskID        int          `json:"subtask_id"`
	SubtaskType      string       `json:"subtask_type"`
	SubtaskSrpm      string       `json:"subtask_srpm"`
	SubtaskSrpmName  string       `json:"subtask_srpm_name"`
	SubtaskSrpmEvr   string       `json:"subtask_srpm_evr"`
	SubtaskDir       string       `json:"subtask_dir"`
	SubtaskTagID     string       `json:"subtask_tag_id"`
	SubtaskTagName   string       `json:"subtask_tag_name"`
	SubtaskTagAuthor string       `json:"subtask_tag_author"`
	SubtaskPackage   string       `json:"subtask_package"`
	SubtaskPkgFrom   string       `json:"subtask_pkg_from"`
	SubtaskChanged   string       `json:"subtask_changed"`
	Type             string       `json:"type"`
	SrcPkgName       string       `json:"src_pkg_name"`
	Archs            []ArchStatus `json:"archs"`
	Approval         []Approval   `json:"approval"`
}

// ArchStatus is the per-architecture build progress of a subtask.
type ArchStatus struct {
	Arch        string `json:"arch"`
	StageStatus string `json:"stage_status"`
}

// Approval is one approve/revoke record attached to a task or subtask.
type Approval struct {
	Type     string `json:"type"`
	Nickname string `json:"nickname"`
	Message  string `json:"message"`
}

// Iteration is one (try, iter) pair of a task's history.
type Iteration struct {
	TaskTry  int `json:"task_try"`
	TaskIter int `json:"task_iter"`
}

// TaskInfoResponse is GET /task/progress/task_info/{id}. It is a bare task
// object rather than a list wrapper.
type TaskInfoResponse = TaskElement

// MaintainersResponse is GET /site/all_maintainers_with_nicknames.
type MaintainersResponse struct {
	RequestArgs map[string]any    `json:"request_args"`
	Length      int               `json:"length"`
	Maintainers []MaintainerEntry `json:"maintainers"`
}

// MaintainerEntry is one packager in the directory.
type MaintainerEntry struct {
	PackagerName     string `json:"packager_name"`
	PackagerNickname string `json:"packager_nickname"`
	CountSourcePkg   int    `json:"count_source_pkg"`
}

// ErrorResponse is the API's standard error body.
type ErrorResponse struct {
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// MaxPackagesPerEvent bounds how many package lines one notification carries
// before the renderer collapses them into "+N more".
const MaxPackagesPerEvent = 64

// ToTask converts a wire record into the domain model. The requested branch is
// passed in because task_repo is authoritative when present but has been seen
// empty in the wild; falling back to the branch we asked for keeps the
// (task_repo, task_id) key stable.
//
// A record whose task_changed cannot be parsed is rejected: the watermark
// comparison depends on it, and silently substituting "now" would either
// suppress a real event or replay an old one.
func (t TaskElement) ToTask(requestedBranch string) (model.Task, error) {
	if t.TaskID <= 0 {
		return model.Task{}, fmt.Errorf("rdb: task with non-positive task_id")
	}
	repo := strings.TrimSpace(t.TaskRepo)
	if repo == "" {
		repo = requestedBranch
	}
	if repo == "" {
		return model.Task{}, fmt.Errorf("rdb: task %d has no task_repo and no branch was requested", t.TaskID)
	}

	changed, err := model.ParseTime(strings.TrimSpace(t.TaskChanged))
	if err != nil {
		return model.Task{}, fmt.Errorf("rdb: task %d: %w", t.TaskID, err)
	}

	out := model.Task{
		TaskID:    t.TaskID,
		TaskRepo:  repo,
		State:     strings.TrimSpace(t.TaskState),
		Owner:     strings.ToLower(strings.TrimSpace(t.TaskOwner)),
		Try:       t.TaskTry,
		Iter:      t.TaskIter,
		Testonly:  t.TaskTestonly,
		Stage:     strings.TrimSpace(t.TaskStage),
		Message:   t.TaskMessage,
		Changed:   changed,
		ChangedAt: t.TaskChanged,
		Packages:  PackageLines(t.Subtasks, MaxPackagesPerEvent),
	}
	if out.State == "" {
		out.State = "UNKNOWN"
	}
	return out, nil
}

// PackageLines renders one human readable line per subtask, de-duplicated and
// capped. The API offers several overlapping ways of naming what a subtask
// builds, so the rules below are ordered from most to least specific.
func PackageLines(subtasks []Subtask, max int) []string {
	if max <= 0 {
		max = MaxPackagesPerEvent
	}
	seen := make(map[string]struct{}, len(subtasks))
	out := make([]string, 0, len(subtasks))

	for _, st := range subtasks {
		line := packageLine(st)
		if line == "" {
			continue
		}
		if _, dup := seen[line]; dup {
			continue
		}
		seen[line] = struct{}{}
		out = append(out, line)
		if len(out) == max {
			break
		}
	}
	return out
}

func packageLine(st Subtask) string {
	switch {
	case st.SubtaskSrpmName != "" && st.SubtaskSrpmEvr != "":
		return strings.TrimSpace(st.SubtaskSrpmName + " " + st.SubtaskSrpmEvr)
	case st.SubtaskSrpmName != "":
		return strings.TrimSpace(st.SubtaskSrpmName)
	case st.SrcPkgName != "":
		if st.SubtaskTagName != "" {
			return strings.TrimSpace(st.SrcPkgName + " " + st.SubtaskTagName)
		}
		return strings.TrimSpace(st.SrcPkgName)
	case st.SubtaskSrpm != "":
		return strings.TrimSuffix(strings.TrimSuffix(st.SubtaskSrpm, ".rpm"), ".src")
	case st.SubtaskTagName != "" && st.SubtaskDir != "":
		return strings.TrimSpace(repoBaseName(st.SubtaskDir) + " " + st.SubtaskTagName)
	case st.SubtaskPackage != "" && st.SubtaskTagName != "":
		return strings.TrimSpace(st.SubtaskPackage + " " + st.SubtaskTagName)
	case st.SubtaskPackage != "":
		return strings.TrimSpace(st.SubtaskPackage)
	case st.SubtaskDir != "":
		return repoBaseName(st.SubtaskDir)
	default:
		return ""
	}
}

// repoBaseName turns "/gears/n/nvidia-nccl.git" into "nvidia-nccl".
func repoBaseName(dir string) string {
	dir = strings.TrimSuffix(strings.TrimSuffix(dir, "/"), ".git")
	return path.Base(dir)
}

// ApprovalSummary renders the approval records of a task, used to explain an
// EPERM: the task is waiting for these people.
func ApprovalSummary(approvals []Approval, max int) []string {
	if max <= 0 {
		max = 8
	}
	out := make([]string, 0, len(approvals))
	for _, a := range approvals {
		if a.Nickname == "" {
			continue
		}
		kind := a.Type
		if kind == "" {
			kind = "approve"
		}
		out = append(out, fmt.Sprintf("%s: %s", kind, a.Nickname))
		if len(out) == max {
			break
		}
	}
	return out
}

// StatesOf collects the distinct non-empty states of a response.
func StatesOf(resp *TasksListResponse) []string {
	if resp == nil {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, 8)
	for _, t := range resp.Tasks {
		st := strings.TrimSpace(t.TaskState)
		if st == "" {
			continue
		}
		if _, dup := seen[st]; dup {
			continue
		}
		seen[st] = struct{}{}
		out = append(out, st)
	}
	return out
}

// OldestChanged returns the smallest task_changed of a response, which is how
// the poller measures how far back its window currently reaches.
func OldestChanged(tasks []model.Task) (time.Time, bool) {
	var oldest time.Time
	found := false
	for _, t := range tasks {
		if !found || t.Changed.Before(oldest) {
			oldest = t.Changed
			found = true
		}
	}
	return oldest, found
}
