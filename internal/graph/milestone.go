package graph

import (
	"strconv"
	"time"

	"github.com/qdrant/go-client/qdrant"

	"github.com/gorizond/vcs-rag/internal/provider"
)

// MilestonePoint builds the milestone node (kind=milestone, T030/US3):
// node_key is the canonical ref (RefMilestone) the in_milestone edges resolve
// to; ms_id repeats its identifying part as an indexed column. due_date is
// written only when the provider gave one (datetime index). Pure: no client,
// no I/O.
func MilestonePoint(meta Meta, ms provider.Milestone) *qdrant.PointStruct {
	msID := strconv.Itoa(ms.ID)
	payload := map[string]any{
		"kind":       string(KindMilestone),
		"node_key":   RefMilestone(meta.Repo, msID),
		"ms_id":      msID,
		"title":      ms.Title,
		"state":      ms.State,
		"group_org":  meta.GroupOrg,
		"repo":       meta.Repo,
		"vcs_type":   meta.VCSType,
		"branch":     meta.Branch,
		"schema_ver": meta.SchemaVer,
		"generation": meta.Generation,
		"indexed_at": time.Now().UTC().Format(time.RFC3339),
	}
	if ms.DueDate != nil {
		payload["due_date"] = ms.DueDate.UTC().Format(time.RFC3339)
	}
	return &qdrant.PointStruct{
		Id: qdrant.NewID(NodeID(Namespace(meta.SchemaVer), meta.Repo, meta.Branch,
			KindMilestone, KeyMilestone(msID)).String()),
		Payload: valueMap(payload),
	}
}

// InMilestoneEdge — work_item → milestone. msID is the string form of the
// milestone node's provider id (the same form MilestonePoint keys on).
func InMilestoneEdge(meta Meta, item provider.WorkItem, msID string) *qdrant.PointStruct {
	return workItemEdge(meta, EdgeInMilestone, item,
		RefMilestone(meta.Repo, msID), "milestone", KeyMilestone(msID))
}
