package work

import (
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/garyburd/redigo/redis"
)

const (
	deadTime          = 10 * time.Second // 2 x heartbeat
	reapPeriod        = 10 * time.Minute
	reapJitterSecs    = 30
	requeueKeysPerJob = 4
)

type deadPoolReaper struct {
	namespace   string
	pool        *redis.Pool
	deadTime    time.Duration
	reapPeriod  time.Duration
	curJobNames []string
	jobTypes    map[string]*jobType

	stopChan         chan struct{}
	doneStoppingChan chan struct{}
}

func newDeadPoolReaper(namespace string, pool *redis.Pool, curJobNames []string, jobTypes map[string]*jobType) *deadPoolReaper {
	return &deadPoolReaper{
		namespace:        namespace,
		pool:             pool,
		deadTime:         deadTime,
		reapPeriod:       reapPeriod,
		curJobNames:      curJobNames,
		jobTypes:         jobTypes,
		stopChan:         make(chan struct{}),
		doneStoppingChan: make(chan struct{}),
	}
}

func (r *deadPoolReaper) start() {
	go r.loop()
}

func (r *deadPoolReaper) stop() {
	r.stopChan <- struct{}{}
	<-r.doneStoppingChan
}

func (r *deadPoolReaper) loop() {
	// Reap immediately after we provide some time for initialization
	timer := time.NewTimer(r.deadTime)
	defer timer.Stop()

	for {
		select {
		case <-r.stopChan:
			r.doneStoppingChan <- struct{}{}
			return
		case <-timer.C:
			// Schedule next occurrence periodically with jitter
			timer.Reset(r.reapPeriod + time.Duration(rand.Intn(reapJitterSecs))*time.Second)

			// Reap
			if err := r.reap(); err != nil {
				logError("dead_pool_reaper.reap", err)
			}
		}
	}
}

func (r *deadPoolReaper) reap() error {
	// Get dead pools
	deadPoolIDs, err := r.findDeadPools()
	if err != nil {
		return err
	}

	conn := r.pool.Get()
	defer conn.Close()

	workerPoolsKey := redisKeyWorkerPools(r.namespace)

	// Cleanup all dead pools
	for deadPoolID, jobNames := range deadPoolIDs {
		lockJobNames := jobNames
		// if we found jobs from the heartbeat, requeue them and remove the heartbeat
		if len(jobNames) > 0 {
			r.requeueInProgressJobs(deadPoolID, jobNames)
			if _, err = conn.Do("DEL", redisKeyHeartbeat(r.namespace, deadPoolID)); err != nil {
				return err
			}
		} else {
			// try to clean up locks for the current set of jobs if heartbeat was not found
			lockJobNames = r.curJobNames
		}
		// Remove dead pool from worker pools set
		if _, err = conn.Do("SREM", workerPoolsKey, deadPoolID); err != nil {
			return err
		}
		// Cleanup any stale lock info
		if err = r.cleanStaleLockInfo(deadPoolID, lockJobNames); err != nil {
			return err
		}
	}

	return nil
}

func (r *deadPoolReaper) cleanStaleLockInfo(poolID string, jobNames []string) error {
	numKeys := len(jobNames) * 2
	redisReapLocksScript := redis.NewScript(numKeys, redisLuaReapStaleLocks)
	var scriptArgs = make([]interface{}, 0, numKeys+1) // +1 for argv[1]

	for _, jobName := range jobNames {
		scriptArgs = append(scriptArgs, redisKeyJobsLock(r.namespace, jobName), redisKeyJobsLockInfo(r.namespace, jobName))
	}
	scriptArgs = append(scriptArgs, poolID) // ARGV[1]

	conn := r.pool.Get()
	defer conn.Close()
	if _, err := redisReapLocksScript.Do(conn, scriptArgs...); err != nil {
		return err
	}

	return nil
}

func (r *deadPoolReaper) requeueInProgressJobs(poolID string, jobNames []string) error {
	jobNames = r.filterNonRetryJobs(jobNames)

	numKeys := len(jobNames) * requeueKeysPerJob
	redisRequeueScript := redis.NewScript(numKeys, redisLuaReenqueueJob)
	var scriptArgs = make([]interface{}, 0, numKeys+1)

	for _, jobName := range jobNames {
		// pops from in progress, push into job queue and decrement the queue lock
		scriptArgs = append(scriptArgs, redisKeyJobsInProgress(r.namespace, poolID, jobName), redisKeyJobs(r.namespace, jobName), redisKeyJobsLock(r.namespace, jobName), redisKeyJobsLockInfo(r.namespace, jobName)) // KEYS[1-4 * N]
	}
	scriptArgs = append(scriptArgs, poolID) // ARGV[1]

	conn := r.pool.Get()
	defer conn.Close()

	// Keep moving jobs until all queues are empty
	for {
		values, err := redis.Values(redisRequeueScript.Do(conn, scriptArgs...))
		if err == redis.ErrNil {
			return nil
		} else if err != nil {
			return err
		}

		if len(values) != 3 {
			return fmt.Errorf("need 3 elements back")
		}
	}
}

func (r *deadPoolReaper) filterNonRetryJobs(jobNames []string) []string {
	filteredJobNames := make([]string, 0)
	for _, jobName := range jobNames {
		jobType, ok := r.jobTypes[jobName]
		if !ok || !jobType.RetryOnStart {
			continue
		}
		filteredJobNames = append(filteredJobNames, jobName)
	}
	return filteredJobNames
}

func (r *deadPoolReaper) findDeadPools() (map[string][]string, error) {
	conn := r.pool.Get()
	defer conn.Close()

	workerPoolsKey := redisKeyWorkerPools(r.namespace)

	workerPoolIDs, err := redis.Strings(conn.Do("SMEMBERS", workerPoolsKey))
	if err != nil {
		return nil, err
	}

	deadPools := map[string][]string{}
	for _, workerPoolID := range workerPoolIDs {
		heartbeatKey := redisKeyHeartbeat(r.namespace, workerPoolID)
		heartbeatAt, err := redis.Int64(conn.Do("HGET", heartbeatKey, "heartbeat_at"))
		if err == redis.ErrNil {
			// heartbeat expired, save dead pool and use cur set of jobs from reaper
			deadPools[workerPoolID] = []string{}
			continue
		}
		if err != nil {
			return nil, err
		}

		// Check that last heartbeat was long enough ago to consider the pool dead
		if time.Unix(heartbeatAt, 0).Add(r.deadTime).After(time.Now()) {
			continue
		}

		jobNameList, err := redis.String(conn.Do("HGET", heartbeatKey, "job_names"))
		if err == redis.ErrNil {
			continue
		}
		if err != nil {
			return nil, err
		}

		deadPools[workerPoolID] = strings.Split(jobNameList, ",")
	}

	return deadPools, nil
}
