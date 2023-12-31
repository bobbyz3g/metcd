// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"fmt"
	"io"
	"metcd/raftnode"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"go.etcd.io/etcd/raft/v3/raftpb"
)

func getSnapshotFn() (func() ([]byte, error), <-chan struct{}) {
	snapshotTriggeredC := make(chan struct{})
	return func() ([]byte, error) {
		snapshotTriggeredC <- struct{}{}
		return nil, nil
	}, snapshotTriggeredC
}

type cluster struct {
	peers              []string
	commitC            []<-chan *raftnode.Commit
	errorC             []<-chan error
	proposePipe        []*raftnode.ProposePipe
	confChangeC        []chan raftpb.ConfChange
	snapshotTriggeredC []<-chan struct{}
}

// newCluster creates a cluster of n nodes
func newCluster(n int) *cluster {
	peers := make([]string, n)
	for i := range peers {
		peers[i] = fmt.Sprintf("http://127.0.0.1:%d", 10000+i)
	}

	clus := &cluster{
		peers:              peers,
		commitC:            make([]<-chan *raftnode.Commit, len(peers)),
		errorC:             make([]<-chan error, len(peers)),
		proposePipe:        make([]*raftnode.ProposePipe, len(peers)),
		confChangeC:        make([]chan raftpb.ConfChange, len(peers)),
		snapshotTriggeredC: make([]<-chan struct{}, len(peers)),
	}

	for i := range clus.peers {
		os.RemoveAll(fmt.Sprintf("metcd-%d", i+1))
		os.RemoveAll(fmt.Sprintf("metcd-%d-snap", i+1))
		clus.confChangeC[i] = make(chan raftpb.ConfChange, 1)
		fn, snapshotTriggeredC := getSnapshotFn()
		clus.snapshotTriggeredC[i] = snapshotTriggeredC
		clus.proposePipe[i] = &raftnode.ProposePipe{ProposeC: make(chan string, 1)}
		rc := raftnode.NewRaftNode(i+1, clus.peers, false, fn, clus.proposePipe[i], clus.confChangeC[i])
		clus.commitC[i] = rc.CommitC()
		clus.errorC[i] = rc.ErrorC()
	}

	return clus
}

// Close closes all cluster nodes and returns an error if any failed.
func (clus *cluster) Close() (err error) {
	for i := range clus.peers {
		go func(i int) {
			for range clus.commitC[i] { //revive:disable-line:empty-block
				// drain pending commits
			}
		}(i)
		clus.proposePipe[i].Close()
		// wait for channel to close
		if erri := <-clus.errorC[i]; erri != nil {
			err = erri
		}
		// clean intermediates
		os.RemoveAll(fmt.Sprintf("metcd-%d", i+1))
		os.RemoveAll(fmt.Sprintf("metcd-%d-snap", i+1))
	}
	return err
}

func (clus *cluster) closeNoErrors(t *testing.T) {
	t.Log("closing cluster...")
	if err := clus.Close(); err != nil {
		t.Fatal(err)
	}
	t.Log("closing cluster [done]")
}

// TestProposeOnCommit starts three nodes and feeds commits back into the proposal
// channel. The intent is to ensure blocking on a proposal won't block raft progress.
func TestProposeOnCommit(t *testing.T) {
	clus := newCluster(3)
	defer clus.closeNoErrors(t)

	donec := make(chan struct{})
	for i := range clus.peers {
		// feedback for "n" committed entries, then update donec
		go func(pC chan<- string, cC <-chan *raftnode.Commit, eC <-chan error) {
			for n := 0; n < 100; n++ {
				c, ok := <-cC
				if !ok {
					pC = nil
				}
				select {
				case pC <- c.Data[0]:
					continue
				case err := <-eC:
					t.Errorf("eC message (%v)", err)
				}
			}
			donec <- struct{}{}
			for range cC { //revive:disable-line:empty-block
				// acknowledge the commits from other nodes so
				// raft continues to make progress
			}
		}(clus.proposePipe[i].ProposeC, clus.commitC[i], clus.errorC[i])

		// one message feedback per node
		go func(i int) {
			clus.proposePipe[i].ProposeC <- "foo"
		}(i)
	}

	for range clus.peers {
		<-donec
	}
}

// TestCloseProposerBeforeReplay tests closing the producer before raft starts.
func TestCloseProposerBeforeReplay(t *testing.T) {
	clus := newCluster(1)
	// close before replay so raft never starts
	defer clus.closeNoErrors(t)
}

// TestCloseProposerInflight tests closing the producer while
// committed messages are being published to the client.
func TestCloseProposerInflight(t *testing.T) {
	clus := newCluster(1)
	defer clus.closeNoErrors(t)

	var wg sync.WaitGroup
	wg.Add(1)

	// some inflight ops
	go func() {
		defer wg.Done()
		clus.proposePipe[0].ProposeC <- "foo"
		clus.proposePipe[0].ProposeC <- "bar"
	}()

	// wait for one message
	if c, ok := <-clus.commitC[0]; !ok || c.Data[0] != "foo" {
		t.Fatalf("Commit failed")
	}

	wg.Wait()
}

func TestPutAndGetKeyValue(t *testing.T) {
	clusters := []string{"http://127.0.0.1:9021"}

	proposePipe := &raftnode.ProposePipe{
		ProposeC: make(chan string),
	}
	defer proposePipe.Close()

	confChangeC := make(chan raftpb.ConfChange)
	defer close(confChangeC)

	var kvs *kvstore
	getSnapshot := func() ([]byte, error) { return kvs.getSnapshot() }
	rc := raftnode.NewRaftNode(1, clusters, false, getSnapshot, proposePipe, confChangeC)
	kvs = newKVStore(<-rc.SnapshotterReady(), proposePipe, rc.CommitC(), rc.ErrorC())

	srv := httptest.NewServer(&httpKVAPI{
		store:       kvs,
		rc:          rc,
		confChangeC: confChangeC,
	})
	defer srv.Close()

	// wait server started
	<-time.After(time.Second * 3)

	wantKey, wantValue := "test-key", "test-value"
	url := fmt.Sprintf("%s/%s", srv.URL, wantKey)
	body := bytes.NewBufferString(wantValue)
	cli := srv.Client()

	req, err := http.NewRequest(http.MethodPut, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/html; charset=utf-8")
	_, err = cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	// wait for a moment for processing message, otherwise get would be failed.
	<-time.After(time.Second)

	resp, err := cli.Get(url)
	if err != nil {
		t.Fatal(err)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if gotValue := string(data); wantValue != gotValue {
		t.Fatalf("expect %s, got %s", wantValue, gotValue)
	}
}

// TestAddNewNode tests adding new node to the existing cluster.
func TestAddNewNode(t *testing.T) {
	clus := newCluster(3)
	defer clus.closeNoErrors(t)

	os.RemoveAll("metcd-4")
	os.RemoveAll("metcd-4-snap")
	defer func() {
		os.RemoveAll("metcd-4")
		os.RemoveAll("metcd-4-snap")
	}()

	newNodeURL := "http://127.0.0.1:10004"
	clus.confChangeC[0] <- raftpb.ConfChange{
		Type:    raftpb.ConfChangeAddNode,
		NodeID:  4,
		Context: []byte(newNodeURL),
	}

	proposePipe := &raftnode.ProposePipe{
		ProposeC: make(chan string),
	}
	defer proposePipe.Close()

	confChangeC := make(chan raftpb.ConfChange)
	defer close(confChangeC)

	raftnode.NewRaftNode(4, append(clus.peers, newNodeURL), true, nil, proposePipe, confChangeC)

	go func() {
		proposePipe.ProposeC <- "foo"
	}()

	if c, ok := <-clus.commitC[0]; !ok || c.Data[0] != "foo" {
		t.Fatalf("Commit failed")
	}
}

func TestSnapshot(t *testing.T) {
	prevDefaultSnapshotCount := raftnode.DefaultSnapshotCount
	prevSnapshotCatchUpEntriesN := raftnode.SnapshotCatchUpEntriesN
	raftnode.DefaultSnapshotCount = 4
	raftnode.SnapshotCatchUpEntriesN = 4
	defer func() {
		raftnode.DefaultSnapshotCount = prevDefaultSnapshotCount
		raftnode.SnapshotCatchUpEntriesN = prevSnapshotCatchUpEntriesN
	}()

	clus := newCluster(3)
	defer clus.closeNoErrors(t)

	go func() {
		clus.proposePipe[0].ProposeC <- "foo"
	}()

	c := <-clus.commitC[0]

	select {
	case <-clus.snapshotTriggeredC[0]:
		t.Fatalf("snapshot triggered before applying done")
	default:
	}
	close(c.ApplyDoneC)
	<-clus.snapshotTriggeredC[0]
}
