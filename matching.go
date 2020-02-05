package idmatch

import (
	"context"
	"fmt"
	"sort"

	"github.com/sirupsen/logrus"
	"gonum.org/v1/gonum/floats"
	simplegraph "gonum.org/v1/gonum/graph"
	"gonum.org/v1/gonum/graph/simple"
	"gonum.org/v1/gonum/graph/topo"
	"gonum.org/v1/gonum/graph/traverse"
	"gonum.org/v1/gonum/stat"

	"github.com/src-d/identity-matching/external"
	"github.com/src-d/identity-matching/reporter"
)

type node struct {
	Value *Person
	id    int64
}

func (g node) ID() int64 {
	return g.id
}

// Int64Slice attaches the methods of Interface to []int64, sorting in increasing order.
type Int64Slice []int64

func (p Int64Slice) Len() int           { return len(p) }
func (p Int64Slice) Less(i, j int) bool { return p[i] < p[j] }
func (p Int64Slice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

// Sort is a convenience method.
func (p Int64Slice) Sort() { sort.Sort(p) }

// addEdgesWithMatcher adds edges by the ground truth from an external matcher.
func addEdgesWithMatcher(people People, peopleGraph *simple.UndirectedGraph,
	matcher external.Matcher) (map[string]struct{}, error) {
	unprocessedEmails := map[string]struct{}{}
	// Add edges by the groundtruth fetched with external matcher.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	username2extID := make(map[string]node)
	var username string
	var err error
	noMatchWarned := map[string]struct{}{}
	for index, person := range people {
		for _, email := range person.Emails {
			if matcher.SupportsMatchingByCommit() && person.SampleCommit != nil {
				username, err = matcher.MatchByCommit(
					ctx, email, person.SampleCommit.Repo, person.SampleCommit.Hash)
			}
			if username == "" {
				username, err = matcher.MatchByEmail(ctx, email)
			}
			if err != nil {
				if err == external.ErrNoMatches {
					pstr := person.String()
					if _, exists := noMatchWarned[pstr]; !exists {
						noMatchWarned[pstr] = struct{}{}
						logrus.Warnf("no matches for person %s", pstr)
					}
				} else {
					logrus.Errorf("unexpected error for person %s: %v", person.String(), err)
				}
				unprocessedEmails[email] = struct{}{}
			} else {
				if person.ExternalID != "" && username != person.ExternalID {
					return unprocessedEmails, fmt.Errorf(
						"person %s has emails with different external ids: %s %s",
						person.String(), person.ExternalID, username)
				}
				person.ExternalID = username
				if val, ok := username2extID[username]; ok {
					err := setEdge(peopleGraph, val, peopleGraph.Node(index).(node))
					if err != nil {
						return unprocessedEmails, nil
					}
				} else {
					username2extID[username] = peopleGraph.Node(int64(index)).(node)
				}
				reporter.Increment("external API emails found")
			}
		}
	}
	err = matcher.OnIdle()
	reporter.Commit("external API components", len(username2extID))
	reporter.Commit("external API emails not found", len(unprocessedEmails))
	return unprocessedEmails, err
}

// ReducePeople merges the identities together by following the fixed set of rules.
// 1. Run the external matching, if available.
// 2. Run the series of heuristics on those items which were left untouched in the list (everything
//    in case of ext == nil, not found in case of ext != nil).
//
// The heuristics are:
// TODO(vmarkovtsev): describe the current approach
func ReducePeople(people People, matcher external.Matcher, blacklist Blacklist,
	maxIdentities int) error {
	peopleGraph := simple.NewUndirectedGraph()
	for index, person := range people {
		peopleGraph.AddNode(node{person, index})
	}

	unmatchedEmails := map[string]struct{}{}
	var err error
	if matcher != nil {
		unmatchedEmails, err = addEdgesWithMatcher(people, peopleGraph, matcher)
		if err != nil {
			return err
		}
	}

	// Add edges by the same unpopular email
	email2id := make(map[string]node)
	for index, person := range people {
		for _, email := range person.Emails {
			if matcher != nil {
				if _, unmatched := unmatchedEmails[email]; !unmatched {
					// Do not process emails which were matched by an external matcher
					continue
				}
			}
			if blacklist.isPopularEmail(email) {
				reporter.Increment("popular emails found")
				continue
			}
			if val, ok := email2id[email]; ok {
				err = setEdge(peopleGraph, val, peopleGraph.Node(index).(node))
				if err != nil {
					return err
				}
			} else {
				email2id[email] = peopleGraph.Node(index).(node)
			}
		}
	}
	reporter.Commit("people matched by email", len(email2id))

	// Add edges by the same unpopular name
	name2id := make(map[string]map[string][]node)
	// We need to sort keys because the algorithm is order dependent
	keys := make([]int64, 0, len(people))
	for k := range people {
		keys = append(keys, k)
	}
	Int64Slice(keys).Sort()
	for _, index := range keys {
		myNode := peopleGraph.Node(index).(node)
		for _, name := range myNode.Value.NamesWithRepos {
			if blacklist.isPopularName(name.String()) {
				reporter.Increment("popular names found")
				continue
			}
			for { // this for is to exit with break from the block when required
				sameNameIDNodes, exists := name2id[name.String()]
				if exists {
					if sameNameAndExternalIDNodes, exists := sameNameIDNodes[myNode.Value.ExternalID]; exists {
						for _, connectedNode := range sameNameAndExternalIDNodes {
							if !passIdentitiesLimit(peopleGraph, maxIdentities, myNode, connectedNode) {
								continue
							}
							err = setEdge(peopleGraph, connectedNode, myNode)
							if err != nil {
								return err
							}
						}
						break
					}
				} else {
					sameNameIDNodes = map[string][]node{}
					name2id[name.String()] = sameNameIDNodes
				}
				sameNameIDNodes[myNode.Value.ExternalID] = append(sameNameIDNodes[myNode.Value.ExternalID], myNode)
				break
			}
		}
	}

	// Merge names with only one found external id
	for _, externalIDs := range name2id {
		if len(externalIDs) == 2 { // one should be empty => merge them
			toMerge := false
			var connected []node
			for externalID, nodes := range externalIDs {
				if externalID == "" {
					toMerge = true
				}
				connected = append(connected, nodes...)
			}
			if toMerge {
				for x, edgeX := range connected {
					for _, edgeY := range connected[x+1:] {
						if !passIdentitiesLimit(peopleGraph, maxIdentities, edgeX, edgeY) {
							continue
						}
						err = setEdge(peopleGraph, edgeX, edgeY)
						// err can occur here and it is fine.
					}
				}
			}
		}
	}

	reporter.Commit("people matched by name", len(name2id))

	var componentsSize []float64
	for _, component := range topo.ConnectedComponents(peopleGraph) {
		var toMerge []int64
		for _, node := range component {
			toMerge = append(toMerge, node.ID())
		}
		componentsSize = append(componentsSize, float64(len(toMerge)))
		_, err := people.Merge(toMerge...)
		if err != nil {
			return err
		}
	}
	mean, std := stat.MeanStdDev(componentsSize, nil)
	if mean != mean {
		mean = 0
	}
	if std != std {
		std = 0
	}
	reporter.Commit("connected component size mean", mean)
	reporter.Commit("connected component size std", std)
	reporter.Commit("connected component size max", floats.Max(componentsSize))
	reporter.Commit("people after reduce", len(people))

	return nil
}

func passIdentitiesLimit(graph *simple.UndirectedGraph, maxIdentities int, node1, node2 node) bool {
	n1Emails, n1Names := componentUniqueEmailsAndNames(graph, node1)
	n2Emails, n2Names := componentUniqueEmailsAndNames(graph, node2)
	if n1Emails+n1Names >= maxIdentities || n2Names+n2Emails >= maxIdentities {
		logrus.Debugf(
			"above the identities limit: %s (%d emails, %d names) and %s (%d emails, %d names)",
			node1.Value.String(), n1Emails, n1Names, node2.Value.String(), n2Emails, n2Names)
		return false
	}
	return true
}

// setEdge propagates ExternalID when you connect two components
func setEdge(graph *simple.UndirectedGraph, node1, node2 node) error {
	externalID1 := node1.Value.ExternalID
	externalID2 := node2.Value.ExternalID
	if externalID1 != "" && externalID2 != "" && externalID1 != externalID2 {
		return fmt.Errorf(
			"cannot set edge between nodes with different ExternalIDs: %s %s",
			externalID1, externalID2)
	}
	var nodeToFix node
	newExternalID := ""
	if externalID1 == "" && externalID2 != "" {
		newExternalID = externalID2
		nodeToFix = node1
	} else if externalID1 != "" && externalID2 == "" {
		newExternalID = externalID1
		nodeToFix = node2
	}
	if newExternalID != "" {
		var w traverse.DepthFirst
		w.Walk(graph, nodeToFix, func(sn simplegraph.Node) bool {
			n := sn.(node)
			if n.Value.ExternalID != "" && n.Value.ExternalID != newExternalID {
				panic(fmt.Errorf(
					"cannot set edge between components with different ExternalIDs: |%s| |%s|",
					newExternalID, n.Value.ExternalID))
			}
			n.Value.ExternalID = newExternalID
			return false
		})
	}

	graph.SetEdge(graph.NewEdge(node1, node2))
	reporter.Increment("graph edges")
	return nil
}

// componentUniqueEmailsAndNames calculates the number of unique emails and names in the component
// with n node inside
func componentUniqueEmailsAndNames(graph *simple.UndirectedGraph, n simplegraph.Node) (int, int) {
	emails := map[string]struct{}{}
	names := map[string]struct{}{}
	var w traverse.DepthFirst
	w.Walk(graph, n, func(sn simplegraph.Node) bool {
		for _, email := range sn.(node).Value.Emails {
			emails[email] = struct{}{}
		}
		for _, name := range sn.(node).Value.NamesWithRepos {
			names[name.String()] = struct{}{}
		}
		return false
	})
	return len(emails), len(names)
}

func setPrimaryValue(people People, freqs map[string]*Frequency, getter func(*Person) []string,
	setter func(*Person, string), minRecentCount int) {
	for _, p := range people {
		recentMaxFreq := 0
		totalMaxFreq := 0
		sumRecentCount := 0
		recentPrimaryValue := ""
		totalPrimaryValue := ""
		for _, value := range getter(p) {
			if freq, ok := freqs[value]; ok {
				sumRecentCount += freq.Recent
				if freq.Recent > recentMaxFreq {
					recentMaxFreq = freq.Recent
					recentPrimaryValue = value
				}
				if freq.Total > totalMaxFreq {
					totalMaxFreq = freq.Total
					totalPrimaryValue = value
				}
			} else {
				logrus.Panicf("freqs does not contain %s key", value)
			}
		}
		if sumRecentCount >= minRecentCount {
			setter(p, recentPrimaryValue)
		} else {
			setter(p, totalPrimaryValue)
		}
	}
}

// SetPrimaryValues sets people primary name and email to the most frequent name and email of
// the person's identity. Stats for the fixed recent period of time are used if there are at least
// minRecentCount commits made by the person's identity in that period. Otherwise the stats
// for all the time are used.
func SetPrimaryValues(people People, nameFreqs, emailFreqs map[string]*Frequency,
	minRecentCount int) {
	setPrimaryValue(people, nameFreqs, func(p *Person) []string {
		names := make([]string, len(p.NamesWithRepos))
		for i, n := range p.NamesWithRepos {
			names[i] = n.Name
		}
		return names
	}, func(p *Person, name string) { p.PrimaryName = name }, minRecentCount)
	setPrimaryValue(people, emailFreqs, func(p *Person) []string { return p.Emails },
		func(p *Person, email string) { p.PrimaryEmail = email }, minRecentCount)
}
