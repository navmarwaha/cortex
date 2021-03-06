package store

import (
	"github.com/golang/glog"
	"github.com/myntra/cortex/pkg/events"
	"github.com/myntra/cortex/pkg/rules"
)

type bucketStorage struct {
	es *eventStorage
	rs *ruleStorage
}

func (b *bucketStorage) stash(ruleID string, event *events.Event) error {
	glog.Info("stash event ==>  ", event)
	if b.es.bucketExists(ruleID) {
		return b.es.stash(rules.Rule{ID: ruleID}, event)
	}

	rule := b.rs.getRule(ruleID)
	return b.es.stash(*rule, event)
}
