package action

import (
	"github.com/jakenesler/navigatorr/store"
	"testing"
)

func TestResumeAdmissionKeepsOfflineReviewAndStopAvailable(t *testing.T) {
	single := &store.ActionInstance{ActionName: "transcode_media"}
	batch := &store.ActionInstance{ActionName: "transcode_batch"}
	for _, decision := range []string{"pause", "cancel", "abort"} {
		if ResumeRequiresWorker(single, decision) || ResumeRequiresWorker(batch, decision) {
			t.Fatal("stop/discard requires an encoder", decision)
		}
	}
	if ResumeRequiresWorker(single, "reject") || !ResumeRequiresWorker(batch, "reject") {
		t.Fatal("discarding a batch child must not advance the next file while offline")
	}
	if ResumeRequiresWorker(single, "accept_loss") || !ResumeRequiresWorker(batch, "accept_loss") {
		t.Fatal("batch continuation must check readiness; existing single candidate review must remain available")
	}
	batch.StateJSON = `{"batch_promotion_plan":{"members":[{"item_key":"verified"}]}}`
	if ResumeRequiresWorker(batch, "approve") || ResumeRequiresWorker(batch, "reject") {
		t.Fatal("final replacement review incorrectly requires encoder")
	}
	if !ResumeRequiresWorker(batch, "resume") || !ResumeRequiresWorker(single, "") {
		t.Fatal("active work bypassed worker readiness")
	}
}
