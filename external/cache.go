package external

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

// CachedUser represents personal name and username of a person
type CachedUser struct {
	User    string
	Matched bool // false if there is no match from the external API
}

// CacheKey is composed of user email and method of searching for it
type CacheKey struct {
	Email	string
	Method	string // byCommit, byEmail, etc.
}

// ByEmailAndMethod implements sort.Interface for []CacheKey
type ByEmailAndMethod []CacheKey

func (a ByEmailAndMethod) Len() int           { return len(a) }
func (a ByEmailAndMethod) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a ByEmailAndMethod) Less(i, j int) bool { if a[i].Email == a[j].Email { return a[i].Method < a[j].Method } else { return a[i].Email < a[j].Email } }

type safeUserCache struct {
	cache     map[CacheKey]CachedUser
	lock      sync.RWMutex // mutex to make cache mapping safe for concurrent use
	cachePath string
}

// CachedMatcher is a wrapper around Matcher with the cache for queried emails.
type CachedMatcher struct {
	matcher Matcher
	cache   safeUserCache
}

const saveFreq int = 20 // Dump cache to file each saveFreq usernames fetched
const csvTrue string = "1"
const csvFalse string = "0"
const byCommit string = "byCommit"
const byEmail string = "byEmail"

// PathExists reports whether a file or directory exists.
func PathExists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// NewCachedMatcher creates a new matcher with a cache for a given matcher interface.
func NewCachedMatcher(matcher Matcher, cachePath string) (*CachedMatcher, error) {
	if cachePath == "" {
		panic("cachePath cannot be empty")
	}
	logrus.WithFields(logrus.Fields{
		"cachePath": cachePath,
	}).Info("caching the external identities")
	cache := safeUserCache{cache: make(map[CacheKey]CachedUser), cachePath: cachePath, lock: sync.RWMutex{}}
	cachedMatcher := &CachedMatcher{matcher: matcher, cache: cache}
	var err error
	if PathExists(cachePath) {
		err = cachedMatcher.LoadCache()
	} else {
		// Dump empty cache to make sure that it is possible to write to the file
		err = cachedMatcher.DumpCache()
	}
	return cachedMatcher, err
}

// LoadCache reads the CachedMatcher cache from disk.
// It is a proxy for safeUserCache.LoadFromDisk() function.
func (m *CachedMatcher) LoadCache() error {
	return m.cache.LoadFromDisk()
}

// DumpCache saves the current CachedMatcher cache on disk.
// It is a proxy for safeUserCache.DumpOnDisk() function.
func (m CachedMatcher) DumpCache() error {
	return m.cache.DumpOnDisk()
}

// OnIdle saves the current CachedMatcher cache on disk.
func (m CachedMatcher) OnIdle() error {
	return m.DumpCache()
}

// MatchByEmail looks in the cache first, and if there is a cache miss, forwards to the underlying Matcher.
func (m *CachedMatcher) MatchByEmail(ctx context.Context, email string) (user string, err error) {
	if username, exists := m.cache.ReadUserFromCache(CacheKey{email,byEmail}); exists {
		if username.Matched {
			return username.User, nil
		}
		return "", ErrNoMatches
	}
	user, err = m.matcher.MatchByEmail(ctx, email)
	if err == nil {
		m.cache.AddUserToCache(CacheKey{email,byEmail}, user, true)
	}
	if err == ErrNoMatches {
		m.cache.AddUserToCache(CacheKey{email,byEmail}, user, false)
	}
	m.cache.lock.Lock()
	if len(m.cache.cache)%saveFreq == 0 {
		err = m.DumpCache()
	}
	m.cache.lock.Unlock()
	return user, err
}

// SupportsMatchingByCommit acts the same as the underlying Matcher.
func (m *CachedMatcher) SupportsMatchingByCommit() bool {
	return m.matcher.SupportsMatchingByCommit()
}

// MatchByCommit looks in the cache first, and if there is a cache miss, forwards to the underlying Matcher.
func (m *CachedMatcher) MatchByCommit(
	ctx context.Context, email, repo, commit string) (user string, err error) {
	if username, exists := m.cache.ReadUserFromCache(CacheKey{email,byCommit}); exists {
		if username.Matched {
			return username.User, nil
		}
		return "", ErrNoMatches
	}
	user, err = m.matcher.MatchByCommit(ctx, email, repo, commit)
	if err == nil {
		m.cache.AddUserToCache(CacheKey{email,byCommit}, user, true)
	}
	if err == ErrNoMatches {
		m.cache.AddUserToCache(CacheKey{email,byCommit}, user, false)
	}
	m.cache.lock.Lock()
	if len(m.cache.cache)%saveFreq == 0 {
		err = m.DumpCache()
	}
	m.cache.lock.Unlock()
	return user, err
}

// Add to cache safely
func (m *safeUserCache) AddUserToCache(key CacheKey, user string, matched bool) {
	m.lock.Lock()
	m.cache[key] = CachedUser{user, matched}
	m.lock.Unlock()
}

// Read from cache safely
func (m safeUserCache) ReadUserFromCache(key CacheKey) (CachedUser, bool) {
	m.lock.RLock()
	defer m.lock.RUnlock()
	val, exists := m.cache[key]
	return val, exists
}

// LoadFromDisk reads the cache contents from FS.
func (m *safeUserCache) LoadFromDisk() error {
	var file *os.File
	file, err := os.Open(m.cachePath)
	if err != nil {
		return err
	}
	defer func() {
		errClose := file.Close()
		if err == nil {
			err = errClose
		}
	}()

	r := csv.NewReader(file)
	header := make(map[string]int)
	rowIndex := 0
	m.lock.Lock()
	defer m.lock.Unlock()
	for {
		record, err := r.Read()
		rowIndex++

		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if len(header) == 0 {
			if len(record) != 4 {
				return fmt.Errorf("invalid CSV file: should have 4 columns")
			}
			for index, name := range record {
				header[name] = index
			}
		} else {
			if len(record) != len(header) {
				return fmt.Errorf("invalid CSV record: %s", strings.Join(record, ","))
			}
			m.cache[CacheKey{record[header["email"]],record[header["method"]]}] = CachedUser{
				record[header["user"]], record[header["match"]] == csvTrue}
		}
	}
	if err == io.EOF {
		err = nil
	}
	return nil
}

// DumpOnDisk saves cache on disk
func (m safeUserCache) DumpOnDisk() error {
	logrus.Infof("writing the external identities cache to %s", m.cachePath)
	var file *os.File
	existing := safeUserCache{cache: make(map[CacheKey]CachedUser), cachePath: m.cachePath, lock: sync.RWMutex{}}
	flag := os.O_CREATE | os.O_WRONLY
	if existing.LoadFromDisk() == nil && len(existing.cache) > 0 {
		flag |= os.O_APPEND
		logrus.Infof("appending to existing %d records", len(existing.cache))
	}
	file, err := os.OpenFile(m.cachePath, flag, 0666)
	if err != nil {
		return err
	}
	defer func() {
		errClose := file.Close()
		if err == nil {
			err = errClose
		}
	}()

	writer := csv.NewWriter(file)
	defer func() {
		writer.Flush()
		if err == nil {
			err = writer.Error()
		}
	}()
	if len(existing.cache) == 0 {
		err = writer.Write([]string{"email", "user", "match", "method"})
		if err != nil {
			return err
		}
	}
	seq := make([]CacheKey, 0, len(m.cache))
	for key := range m.cache {
		seq = append(seq, key)
	}
	sort.Sort(ByEmailAndMethod(seq))
	written := 0
	for _, key := range seq {
		username := m.cache[key]
		if eusername, exists := existing.cache[key]; exists && eusername == username {
			continue
		}
		match := csvFalse
		if username.Matched {
			match = csvTrue
		}
		err = writer.Write([]string{key.Email, username.User, match, key.Method})
		if err != nil {
			return err
		}
		written++
	}
	logrus.Infof("written %d new records", written)
	return nil
}
