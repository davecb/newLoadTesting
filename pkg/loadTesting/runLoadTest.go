package loadTesting

// Run a load test from a script in "perf" format.
// input looks like "01-Mar-2017 16:00:00 0 0 0 0 path 404 GET"

import (
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"strings"
	"time"
)

// The protocols supported by the library
const (
	FilesystemProtocol  = iota
	RESTProtocol        // Eg, RCDN's http-based REST Protocol
	S3Protocol
	CephProtocol
)

// These are the field names in the csv file
const ( // nolint
	dateField         = iota // nolint
	timeField                // nolint
	latencyField             // nolint
	transferTimeField        // nolint
	thinkTimeField           // nolint
	bytesField
	pathField
	returnCodeField
	operatorField
)

// Config contains all the optional parameters.
type Config struct {
	Verbose      bool
	Debug        bool
	Serialize    bool
	Cache        bool
	RealTime     bool
	Protocol     int
	Strip        string
	Timeout      time.Duration
	StepDuration int
	HostHeader   string
}


var conf Config
var random = rand.New(rand.NewSource(42))
var pipe = make(chan []string, 100)
var alive = make(chan bool, 1000)

var junkDataFile = "/tmp/LoadTestJunkDataFile" // FIXME for write and r/w tests
const size = 396759652 // nolint // FIXME, this is a heuristic

// RunLoadTest does whatever main figured out that the caller wanted.
func RunLoadTest(f io.Reader, filename string, fromTime, forTime int,
	tpsTarget, progressRate, startTps int, baseURL string, cfg Config) {
	var processed = 0
	conf = cfg

	if conf.Debug {
		log.Printf("new runLoadTest(f, tpsTarget=%d, progressRate=%d, " +
			"startTps=%d, fromTime=%d, forTime=%d, baseURL=%s)\n",
			tpsTarget, progressRate, startTps, fromTime, forTime, baseURL)
	}

	doPrepWork(baseURL)           // Named "init" fucntion, creates junkDataFile
	defer os.Remove(junkDataFile) // nolint

	go workSelector(f, filename, fromTime, forTime, pipe)    // which pipes work to ...
	go generateLoad(pipe, tpsTarget, progressRate, startTps, baseURL)  // which then writes to "alive"
	for {
		select {
		case <-alive:
			processed++

		case <-time.After(time.Second * conf.Timeout):
			log.Printf("%d records processed\n", processed)
			log.Printf("No activity after %d seconds, halting normally.\n",
				conf.Timeout)
			os.Exit(0)
		}
	}
}

// workSelector pipes a selection from a file to the workers
func workSelector(f io.Reader, filename string, startFrom, runFor int, pipe chan []string) {

	if conf.Debug {
		log.Printf("in workSelector(r, %s, startFrom=%d runFor=%d, pipe)\n", filename, startFrom, runFor)
	}
	r := csv.NewReader(f)
	r.Comma = ' '
	r.Comment = '#'
	r.FieldsPerRecord = -1 // ignore differences

	skipForward(startFrom, r, filename)
	recNo, pipe := copyToPipe(runFor, r, filename, pipe)
	log.Printf("Loaded %d records, closing input\n", recNo)
	close(pipe)
}

// copyToPipe sends work to the workers
func copyToPipe(runFor int, r *csv.Reader, filename string, pipe chan []string) (int, chan []string) {
	// From there, copy to pipe
	recNo := 0
	for ; recNo < runFor; recNo++ {
		record, err := r.Read()
		if err == io.EOF {
			if conf.RealTime {
				// just keep reading
				time.Sleep(time.Millisecond)
				continue
			}
			//log.Printf("At EOF on %s, no new work to queue\n", filename)
			break
		}
		if err != nil {
			log.Fatalf("Fatal error mid-way in %s: %s\n", filename, err)
		}
		if conf.Strip != "" {
			record[pathField] = strings.Replace(record[pathField], conf.Strip, "", 1)
		}
		//log.Printf("writing %v to pipe\n", record)
		pipe <- record
	}
	return recNo, pipe
}

// generateLoad starts progressRate new threads every 10 seconds until we hit progressRate
func generateLoad(pipe chan []string, tpsTarget, progressRate, startTps int, urlPrefix string) {
	if conf.Debug {
		log.Printf("generateLoad(pipe, tpsTarget=%d, progressRate=%d, from, for, prefix\n",
			tpsTarget, progressRate)
	}

	fmt.Print("#yyy-mm-dd hh:mm:ss latency xfertime thinktime bytes url rc\n")
	var closed = make(chan bool)
	switch {
	case conf.RealTime: // progress rate is defined by the input stream
		runRealTimeLoad(pipe, closed, urlPrefix)
	case progressRate != 0:
		runProgressivelyIncreasingLoad(progressRate, tpsTarget, startTps, pipe, closed, urlPrefix)
	case tpsTarget != 0:
		runSteadyLoad(tpsTarget, pipe, closed, urlPrefix)
	case tpsTarget < 0:
		log.Fatal("A zero or negative tps target is not meaningfull, halting\n")
	}
}

// run at a steady tps until the end of the data
func runSteadyLoad(tpsTarget int, pipe chan []string, closed chan bool, urlPrefix string) {
	log.Printf("starting, at %d requests/second\n", tpsTarget)
	// start tpsTarget workers
	for i := 0; i < tpsTarget; i++ {
		go worker(pipe, closed, urlPrefix)
	}
}

// run at whatever load comes down the pipe, used for running in
// parallel to an existing system
func runRealTimeLoad(pipe chan []string, closed chan bool, urlPrefix string) {
	log.Print("starting to read the input file continuously, ^C to stop\n")
	for i := 0; i < 3; i++ {
		// The "3" is a heuristic
		go worker(pipe, closed, urlPrefix)
	}
}

// runProgressivelyIncreasingLoad, the classic load test
func runProgressivelyIncreasingLoad(progressRate, tpsTarget, startTps int, pipe chan []string,
	closed chan bool, urlPrefix string) {

	// start the first workers
	if startTps == 0 {
		startTps = progressRate
	}
	rate := startTps
	for i := 0; i < startTps; i++ {
		go worker(pipe, closed, urlPrefix)
	}
	// add to the workers until we have enough
	log.Printf("now at %d requests/second\n", rate)
	for range time.Tick(time.Duration(conf.StepDuration) * time.Second) { // nolint
		//start another progressRate of workers
		rate += progressRate
		if rate > tpsTarget {
			// OK, we're past the range, quit.
			log.Printf("completed maximum rate, starting %d sec cleanup timer\n", conf.Timeout)
			break
		}
		for i := 0; i < progressRate; i++ {
			go worker(pipe, closed, urlPrefix)
		}
		log.Printf("now at %d requests/second\n", rate)
	}
	// let them run for a cycle and shut down
	time.Sleep(time.Duration(10 * float64(time.Second)))
	close(closed) // We're done
}


// worker reads and executes a task every second until it hits eof
func worker(pipe chan []string, closed chan bool, urlPrefix string) {
	if conf.Debug {
		log.Print("started a worker\n")
	}
	// wait a random fraction of one second before looping, for randomness.
	time.Sleep(time.Duration(random.Float64() * float64(time.Second)))

	for range time.Tick(1 * time.Second) { // nolint
		var r []string

		select {
		case <-closed:
			//log.Print("pipe closed, no more requests to send.\n")
			return
		case r = <-pipe:
			//log.Printf("got %v\n", r)
		}

		switch {
		case r == nil:
			//log.Print("worker reached EOF, no more requests to send.\n")
			return
		case len(r) != 9:
			// bad input data, crash please.
			log.Fatalf("number of fields != 9 in %v", r)
		case r[operatorField] == "GET":
			if conf.Serialize {
				// force this NOT to be asynchronous, for load tests only
				getJunkFile(urlPrefix, r[pathField])
			} else {
				go getJunkFile(urlPrefix, r[pathField])
			}
		case r[operatorField] == "PUT":
			// FIXME: treat PUT as a no-op
		default:
			log.Fatal("operations other than GET and PUT are not implemented yet\n")
		}
	}
}

// putJunkFile sends a specified number of bytes via a PUT
func putJunkFile(baseURL, path string, size int64) { // nolint
	var err error

	if conf.Debug {
		log.Printf("in putJunkFile(%s, %s, %d)\n", baseURL, path, size)
	}
	switch conf.Protocol {
	case RESTProtocol:
		err = RestPut(baseURL, path, size)
	case S3Protocol:
		err = AmazonS3Put(baseURL, path, size) // nolint
	default:
		err = fmt.Errorf("protocol %d not implemented yet", conf.Protocol)
	}
	if err != nil {
		log.Fatalf("Faial error in putJunkFile, %v\n", err)
	}
	// alive <- true
}

// get a url and then throw it away.
func getJunkFile(baseURL, path string) {
	if conf.Debug {
		log.Printf("in getJunkFile(%s, %s), protocol=%v\n", baseURL, path, conf.Protocol)
	}

	switch conf.Protocol {
	case RESTProtocol:
		RestGet(baseURL, path)
	case S3Protocol:
		//MinioS3Get(baseURL, path)
		AmazonS3Get(baseURL, path)
	default:
		log.Fatalf("Protocol %d not implemented yet\n", conf.Protocol)
	}
	// alive <- true
}

// doPrepWork makes sure we have the prerequisites by protocol
func doPrepWork(baseURL string) {
	//MustCreateFilesystemFile(junkDataFile, size)  FXIME. needed for PUT
	switch conf.Protocol {
	case S3Protocol:
		AmazonS3Prep(baseURL)
	}
}
