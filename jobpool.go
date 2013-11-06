// Copyright 2013 Ardan Studios. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
	Package jobpool implements a pool of go routines that are dedicated to processing jobs posted into the pool.
	The jobpool maintains two queues, a normal processing queue and a priority queue. Jobs placed in the priority queue will be processed
	ahead of pending jobs in the normal queue.

	If priority is not required, using ArdanStudios/workpool is faster and more efficient.

		Read the following blog post for more information:blogspot
		http://www.goinggo.net/2013/05/thread-pooling-in-go-programming.html

	New Parameters

	The following is a list of parameters for creating a JobPool:

		numberOfRoutines: Sets the number of job routines that are allowed to process jobs concurrently
		queueCapacity:    Sets the maximum number of pending job objects that can be in queue

	JobPool Management

	Go routines are used to manage and process all the jobs. A single Queue routine provides the safe queuing of work.
	The Queue routine keeps track of the number of jobs in the queue and reports an error if the queue is full.

	The numberOfRoutines parameter defines the number of job routines to create. These job routines will process work
	subbmitted to the queue. The job routines keep track of the number of active job routines for reporting.

	The QueueJob method is used to queue a job into one of the two queues. This call will block until the Queue routine reports back
	success or failure that the job is in queue.

	Example Use Of JobPool

	The following shows a simple test application

		package main

		import (
		    "github.com/goinggo/jobpool"
		    "fmt"
		    "time"
		)

		type WorkProvider1 struct {
		    Name string
		}

		func (this *WorkProvider1) RunJob(jobRoutine int) {

		    fmt.Printf("Perform Job : Provider 1 : Started: %s\n", this.Name)
		    time.Sleep(2 * time.Second)
		    fmt.Printf("Perform Job : Provider 1 : DONE: %s\n", this.Name)
		}

		type WorkProvider2 struct {
		    Name string
		}

		func (this *WorkProvider2) RunJob(jobRoutine int) {

		    fmt.Printf("Perform Job : Provider 2 : Started: %s\n", this.Name)
		    time.Sleep(5 * time.Second)
		    fmt.Printf("Perform Job : Provider 2 : DONE: %s\n", this.Name)
		}

		func main() {

		    jobPool := jobpool.New(2, 1000)

		    jobPool.QueueJob("main", &WorkProvider1{"Normal Priority : 1"}, false)

		    fmt.Printf("*******> QW: %d  AR: %d\n", jobPool.QueuedJobs(), jobPool.ActiveRoutines())
		    time.Sleep(1 * time.Second)

		    jobPool.QueueJob("main", &WorkProvider1{"Normal Priority : 2"}, false)
		    jobPool.QueueJob("main", &WorkProvider1{"Normal Priority : 3"}, false)

		    jobPool.QueueJob("main", &WorkProvider2{"High Priority : 4"}, true)
		    fmt.Printf("*******> QW: %d  AR: %d\n", jobPool.QueuedJobs(), jobPool.ActiveRoutines())

		    time.Sleep(15 * time.Second)

		    jobPool.Shutdown("main")
		}

	Example Output

	The following shows some sample output

		*******> QW: 1  AR: 0
		Perform Job : Provider 1 : Started: Normal Priority : 1
		Perform Job : Provider 1 : Started: Normal Priority : 2
		*******> QW: 2  AR: 2
		Perform Job : Provider 1 : DONE: Normal Priority : 1
		Perform Job : Provider 2 : Started: High Priority : 4

*/
package jobpool

import (
	"container/list"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

//** NEW TYPES

// queueJob is a control structure for queuing jobs
type queueJob struct {
	Jobber                   // The object to execute the job routine against
	Priority      bool       // If the job needs to be placed on the priority queue
	ResultChannel chan error // Used to inform the queue operaion is complete
}

// dequeueJob is a control structure for dequeuing jobs
type dequeueJob struct {
	ResultChannel chan *queueJob // Used to return the queued job to be processed
}

// JobPool maintains queues and Go routines for processing jobs
type JobPool struct {
	priorityJobQueue     *list.List       // The priority job queue
	normalJobQueue       *list.List       // The normal job queue
	queueChannel         chan *queueJob   // Channel allows the thread safe placement of jobs into the queue
	dequeueChannel       chan *dequeueJob // Channel allows the thread safe removal of jobs from the queue
	shutdownQueueChannel chan string      // Channel used to shutdown the queue routine
	jobChannel           chan string      // Channel to signal to a job routine to process a job
	shutdownJobChannel   chan struct{}    // Channel used to shutdown the job routines
	shutdownWaitGroup    sync.WaitGroup   // The WaitGroup for shutting down existing routines
	queuedJobs           int32            // The number of pending jobs in queued
	activeRoutines       int32            // The number of routines active
	queueCapacity        int32            // The max number of jobs we can store in the queue
}

//** INTERFACES

// Jobber is an interface that is implemented to run jobs
type Jobber interface {
	RunJob(jobRoutine int)
}

//** PUBLIC FUNCTIONS

// New creates a new JobPool
//  numberOfRoutines: Sets the number of job routines that are allowed to process jobs concurrently
//  queueCapacity: Sets the maximum number of pending work objects that can be in queue
func New(numberOfRoutines int, queueCapacity int32) (jobPool *JobPool) {
	// Create the job queue
	jobPool = &JobPool{
		priorityJobQueue:     list.New(),
		normalJobQueue:       list.New(),
		queueChannel:         make(chan *queueJob),
		dequeueChannel:       make(chan *dequeueJob),
		shutdownQueueChannel: make(chan string),
		jobChannel:           make(chan string, queueCapacity),
		shutdownJobChannel:   make(chan struct{}),
		queuedJobs:           0,
		activeRoutines:       0,
		queueCapacity:        queueCapacity,
	}

	// Launch the job routines to process work
	for jobRoutine := 0; jobRoutine < numberOfRoutines; jobRoutine++ {
		// Add the routine to the wait group
		jobPool.shutdownWaitGroup.Add(1)

		// Start the job routine
		go jobPool.jobRoutine(jobRoutine)
	}

	// Start the queue routine to capture and provide jobs
	go jobPool.queueRoutine()

	return jobPool
}

//** PUBLIC MEMBER FUNCTIONS

// Shutdown will release resources and shutdown all processing
func (this *JobPool) Shutdown(goRoutine string) (err error) {
	defer catchPanic(&err, goRoutine, "jobPool.JobPool", "Shutdown")

	writeStdout(goRoutine, "jobPool.JobPool", "Shutdown", "Started")
	writeStdout(goRoutine, "jobPool.JobPool", "Shutdown", "Queue Routine")

	this.shutdownQueueChannel <- "Shutdown"
	<-this.shutdownQueueChannel

	close(this.shutdownQueueChannel)
	close(this.queueChannel)
	close(this.dequeueChannel)

	writeStdout(goRoutine, "jobPool.JobPool", "Shutdown", "Shutting Down Job Routines")

	// Close the channel to shut things down
	close(this.shutdownJobChannel)
	this.shutdownWaitGroup.Wait()

	close(this.jobChannel)

	writeStdout(goRoutine, "jobPool.JobPool", "Shutdown", "Completed")
	return err
}

// QueueJob queues a job to be processed
//  jober: An object that implements the Jobber interface
//  priority: If true the job is placed in the priority queue
func (this *JobPool) QueueJob(goRoutine string, jober Jobber, priority bool) (err error) {
	defer catchPanic(&err, goRoutine, "jobPool.JobPool", "QueueJob")

	// Create the job object to queue
	jobPool := &queueJob{
		jober,            // Jobber Interface
		priority,         // Priority
		make(chan error), // Result Channel
	}

	defer close(jobPool.ResultChannel)

	// Queue the job
	this.queueChannel <- jobPool
	err = <-jobPool.ResultChannel

	return err
}

// QueuedJobs will return the number of jobs items in queue
func (this *JobPool) QueuedJobs() int32 {
	return atomic.AddInt32(&this.queuedJobs, 0)
}

// ActiveRoutines will return the number of routines performing work
func (this *JobPool) ActiveRoutines() int32 {

	return atomic.AddInt32(&this.activeRoutines, 0)
}

//** PRIVATE FUNCTIONS

// catchPanic is used to catch any Panic and log exceptions to Stdout. It will also write the stack trace
//  err: A reference to the err variable to be returned to the caller. Can be nil
func catchPanic(err *error, goRoutine string, namespace string, functionName string) {
	if r := recover(); r != nil {

		// Capture the stack trace
		buf := make([]byte, 10000)
		runtime.Stack(buf, false)

		writeStdoutf(goRoutine, namespace, functionName, "PANIC Defered [%v] : Stack Trace : %v", r, string(buf))

		if err != nil {
			*err = fmt.Errorf("%v", r)
		}
	}
}

// writeStdout is used to write a system message directly to stdout
func writeStdout(goRoutine string, namespace string, functionName string, message string) {
	fmt.Printf("%s : %s : %s : %s : %s\n", time.Now().Format("2006-01-02T15:04:05.000"), goRoutine, namespace, functionName, message)
}

// writeStdoutf is used to write a formatted system message directly stdout
func writeStdoutf(goRoutine string, namespace string, functionName string, format string, a ...interface{}) {
	writeStdout(goRoutine, namespace, functionName, fmt.Sprintf(format, a...))
}

//** PRIVATE MEMBER FUNCTIONS

// queueRoutine performs the thread safe queue related processing
func (this *JobPool) queueRoutine() {
	for {

		select {
		case <-this.shutdownQueueChannel:
			writeStdout("Queue", "jobpool.JobPool", "queueRoutine", "Going Down")

			this.shutdownQueueChannel <- "Down"
			return

		case queueJob := <-this.queueChannel:
			// Enqueue the job
			this.queueRoutineEnqueue(queueJob)
			break

		case dequeueJob := <-this.dequeueChannel:
			// Dequeue a job
			this.queueRoutineDequeue(dequeueJob)
			break
		}
	}
}

// queueRoutineEnqueue places a job on either the normal or priority queue
func (this *JobPool) queueRoutineEnqueue(queueJob *queueJob) {
	defer catchPanic(nil, "Queue", "jobPool.JobPool", "queueRoutineEnqueue")

	// If the queue is at capacity don't add it
	if atomic.AddInt32(&this.queuedJobs, 0) == this.queueCapacity {
		queueJob.ResultChannel <- fmt.Errorf("Job Pool At Capacity")
		return
	}

	if queueJob.Priority == true {
		this.priorityJobQueue.PushBack(queueJob)
	} else {
		this.normalJobQueue.PushBack(queueJob)
	}

	// Increment the queued work count
	atomic.AddInt32(&this.queuedJobs, 1)

	// Tell the caller the work is queued
	queueJob.ResultChannel <- nil

	// Tell the job routine to wake up
	this.jobChannel <- "Wake Up"
}

// queueRoutineDequeue remove a job from the queue
func (this *JobPool) queueRoutineDequeue(dequeueJob *dequeueJob) {
	defer catchPanic(nil, "Queue", "jobPool.JobPool", "queueRoutineDequeue")

	var nextJob *list.Element

	if this.priorityJobQueue.Len() > 0 {
		nextJob = this.priorityJobQueue.Front()
		this.priorityJobQueue.Remove(nextJob)
	} else {
		nextJob = this.normalJobQueue.Front()
		this.normalJobQueue.Remove(nextJob)
	}

	// Decrement the queued work count
	atomic.AddInt32(&this.queuedJobs, -1)

	// Cast the list element back to a Job
	jobPool := nextJob.Value.(*queueJob)

	// Give the caller the work to process
	dequeueJob.ResultChannel <- jobPool
}

// jobRoutine performs the actual processing of jobs
func (this *JobPool) jobRoutine(jobRoutine int) {
	for {

		select {
		// Shutdown the job routine
		case <-this.shutdownJobChannel:
			writeStdout(fmt.Sprintf("JobRoutine %d", jobRoutine), "jobPool.JobPool", "jobRoutine", "Going Down")

			this.shutdownWaitGroup.Done()
			return

		// Perform the work
		case <-this.jobChannel:
			this.doJobSafely(jobRoutine)
			break
		}
	}
}

// dequeueJob pulls a job from the queue
func (this *JobPool) dequeueJob() (job *queueJob, err error) {
	defer catchPanic(&err, "jobRoutine", "jobPool.JobPool", "dequeueJob")

	// Create the job object to queue
	requestJob := &dequeueJob{
		ResultChannel: make(chan *queueJob), // Result Channel
	}

	defer close(requestJob.ResultChannel)

	// Dequeue the job
	this.dequeueChannel <- requestJob
	job = <-requestJob.ResultChannel

	return job, err
}

// doJobSafely will executes the job within a safe context
//  jobRoutine: The internal id of the job routine
func (this *JobPool) doJobSafely(jobRoutine int) {
	defer catchPanic(nil, "jobRoutine", "jobPool.JobPool", "doJobSafely")
	defer func() {
		atomic.AddInt32(&this.activeRoutines, -1)
	}()

	// Update the active routine count
	atomic.AddInt32(&this.activeRoutines, 1)

	// Dequeue a job
	queueJob, err := this.dequeueJob()

	if err != nil {
		writeStdoutf("Queue", "jobpool.JobPool", "doJobSafely", "ERROR : %s", err)
		return
	}

	// Perform the job
	queueJob.RunJob(jobRoutine)
}