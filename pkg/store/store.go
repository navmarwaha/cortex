package store

import (
	"fmt"
	"time"

	"github.com/hashicorp/raft-boltdb"
	"github.com/satori/go.uuid"

	"github.com/golang/glog"
	"github.com/hashicorp/raft"
	"github.com/myntra/cortex/pkg/config"
	"github.com/myntra/cortex/pkg/events"
	"github.com/myntra/cortex/pkg/executions"
	"github.com/myntra/cortex/pkg/rules"

	"github.com/myntra/cortex/pkg/js"
	"github.com/myntra/cortex/pkg/util"
	"net/url"
)

const (
	retainSnapshotCount = 2
	raftTimeout         = 10 * time.Second
)

type defaultStore struct {
	opt                  *config.Config
	boltDB               *raftboltdb.BoltStore
	raft                 *raft.Raft
	scriptStorage        *scriptStorage
	bucketStorage        *bucketStorage
	executionStorage     *executionStorage
	executionBucketQueue chan *events.Bucket
	quitFlusherChan      chan struct{}
	persisters           []persister
	restorers            map[MessageType]restorer
}

func newStore(opt *config.Config) (*defaultStore, error) {

	// register persisters
	var persisters []persister
	persisters = append(persisters, persistRules, persistRecords, persistScripts)

	restorers := make(map[MessageType]restorer)

	restorers[RuleType] = restoreRules
	restorers[RecordType] = restoreRecords
	restorers[ScriptType] = restoreScripts

	store := &defaultStore{
		scriptStorage: &scriptStorage{
			m: make(map[string]*js.Script),
		},
		executionStorage: &executionStorage{
			m: make(map[string]*executions.Record),
		},
		bucketStorage: &bucketStorage{
			es: &eventStorage{
				m: make(map[string]*events.Bucket),
			},
			rs: &ruleStorage{
				m: make(map[string]*rules.Rule),
			},
		},
		opt:                  opt,
		quitFlusherChan:      make(chan struct{}),
		executionBucketQueue: make(chan *events.Bucket, 1000),
		persisters:           persisters,
		restorers:            restorers,
	}

	return store, nil
}

func (d *defaultStore) executor() {
	for {
		select {
		case rb := <-d.executionBucketQueue:
			glog.Infof("received bucket %+v", rb)
			go func(rb *events.Bucket) {
				statusCode := 0
				var noScriptResult bool
				result := js.Execute(d.getScript(rb.Rule.ScriptID), rb)
				glog.Infof("Result of the script execution \n%v", result)
				if result == nil {
					noScriptResult = true
				}
				if _, err := url.ParseRequestURI(rb.Rule.HookEndpoint); err != nil {
					glog.Info("Invalid HookEndpoint. Skipping post request")
				} else {
					if noScriptResult {
						statusCode = util.RetryPost(rb, rb.Rule.HookEndpoint, rb.Rule.HookRetry)
					} else {
						statusCode = util.RetryPost(result, rb.Rule.HookEndpoint, rb.Rule.HookRetry)
					}
				}

				id := uuid.NewV4()
				record := &executions.Record{
					ID:             id.String(),
					Bucket:         *rb,
					ScriptResult:   result,
					HookStatusCode: statusCode,
					CreatedAt:      time.Now(),
				}

				glog.Infof("addRecord %v\n", record)
				glog.Infoln("err ", d.addRecord(record))

			}(rb)
		}
	}
}

func (d *defaultStore) flusher() {

	go d.executor()

	ticker := time.NewTicker(time.Millisecond * time.Duration(d.opt.FlushInterval))
loop:
	for {
		select {
		case <-ticker.C:
			if d.raft.State() != raft.Leader {
				glog.Info("node is not leader, skipping flush")
				continue
			}
			for ruleID, bucket := range d.bucketStorage.es.clone() {
				glog.Infof("rule flusher ==> %v with size %v canflush ? %v, can flush in %v",
					ruleID, len(bucket.Events), bucket.CanFlush(), bucket.CanFlushIn())
				if bucket.CanFlush() {
					go func() {
						glog.Infof("post bucket to execution %+v ", bucket)
						d.executionBucketQueue <- bucket

						err := d.flushBucket(ruleID)
						if err != nil {
							glog.Errorf("error flushing bucket %v %v", ruleID, err)
						}
					}()
				}
			}
		case <-d.quitFlusherChan:
			break loop
		}
	}

}

func (d *defaultStore) expirer() {
	ticker := time.NewTicker(time.Hour)

	for {
		select {
		case <-ticker.C:
			if d.raft.State() != raft.Leader {
				glog.Info("node is not leader, skipping expire")
				continue
			}
			if d.executionStorage.getTotalRecordsCount() > d.opt.MaxHistory {
				// TODO, remove oldest records
			}
		}

	}
}

func (d *defaultStore) applyCMD(cmd Command) error {
	if d.raft.State() != raft.Leader {
		return fmt.Errorf("not leader")
	}

	glog.Infof("apply cmd %v\n marshalling", cmd)

	b, err := cmd.MarshalMsg(nil)
	if err != nil {
		glog.Errorf("stash %v err %v\n", cmd, err)
		return err
	}

	glog.Infof("==> apply %+v\n", cmd)
	f := d.raft.Apply(b, raftTimeout)
	return f.Error()
}

func (d *defaultStore) matchAndStash(event *events.Event) error {
	glog.Info("match and stash event ==>  ", event)
	for _, rule := range d.getRules() {
		go d.match(rule, event)
	}
	return nil
}

func (d *defaultStore) match(rule *rules.Rule, event *events.Event) error {
	glog.Info("match event ==>  ", event)
	if rule.HasMatching(event.EventType) {
		go d.stash(rule.ID, event)
	}
	return nil

}

func (d *defaultStore) stash(ruleID string, event *events.Event) error {
	glog.Info("apply stash event ==>  ", event)
	return d.applyCMD(Command{
		Op:     "stash",
		RuleID: ruleID,
		Event:  event,
	})
}

func (d *defaultStore) addRule(rule *rules.Rule) error {

	if rule.Dwell == 0 || rule.DwellDeadline == 0 || rule.MaxDwell == 0 {
		rule.Dwell = d.opt.DefaultDwell
		rule.DwellDeadline = d.opt.DefaultDwellDeadline
		rule.MaxDwell = d.opt.DefaultMaxDwell
	}

	return d.applyCMD(Command{
		Op:   "add_rule",
		Rule: rule,
	})
}

func (d *defaultStore) updateRule(rule *rules.Rule) error {
	return d.applyCMD(Command{
		Op:   "update_rule",
		Rule: rule,
	})
}

func (d *defaultStore) addScript(script *js.Script) error {
	return d.applyCMD(Command{
		Op:     "add_script",
		Script: script,
	})
}

func (d *defaultStore) updateScript(script *js.Script) error {
	return d.applyCMD(Command{
		Op:     "update_script",
		Script: script,
	})
}

func (d *defaultStore) removeScript(id string) error {
	return d.applyCMD(Command{
		Op:       "remove_script",
		ScriptID: id,
	})
}

func (d *defaultStore) removeRule(ruleID string) error {
	return d.applyCMD(Command{
		Op:     "remove_rule",
		RuleID: ruleID,
	})
}

func (d *defaultStore) flushBucket(ruleID string) error {
	return d.applyCMD(Command{
		Op:     "flush_bucket",
		RuleID: ruleID,
	})
}

func (d *defaultStore) addRecord(r *executions.Record) error {
	return d.applyCMD(Command{
		Op:     "add_record",
		Record: r,
	})
}

func (d *defaultStore) removeRecord(id string) error {
	return d.applyCMD(Command{
		Op:       "remove_record",
		RecordID: id,
	})
}

func (d *defaultStore) getScripts() []string {
	return d.scriptStorage.getScripts()
}

func (d *defaultStore) getScript(id string) *js.Script {
	return d.scriptStorage.getScript(id)
}

func (d *defaultStore) getRules() []*rules.Rule {
	return d.bucketStorage.rs.getRules()
}

func (d *defaultStore) getRule(ruleID string) *rules.Rule {
	return d.bucketStorage.rs.getRule(ruleID)
}

func (d *defaultStore) getRecords(ruleID string) []*executions.Record {
	return d.executionStorage.getRecords(ruleID)
}

func (d *defaultStore) getRecordsCount(ruleID string) int {
	return d.executionStorage.getRecordsCount(ruleID)
}
