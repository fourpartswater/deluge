package deluge

import (
	"bufio"
	"fmt"
	"io"

	"github.com/unchartedsoftware/plog"
	"gopkg.in/olivere/elastic.v3"

	"github.com/unchartedsoftware/deluge/equalizer"
	"github.com/unchartedsoftware/deluge/pool"
	"github.com/unchartedsoftware/deluge/threshold"
	"github.com/unchartedsoftware/deluge/util/progress"
)

// Ingestor is an Elasticsearch ingestor client. Create one by calling
// NewIngestor.
type IngestorV3 struct {
	input                Input
	documentCtor         Constructor
	index                string
	client               *elastic.Client
	clearExisting        bool
	numActiveConnections int
	numWorkers           int
	numReplicas          int
	compression          string
	threshold            float64
	bulkByteSize         int64
	scanBufferSize       int
}

// NewIngestor instantiates and configures a new Ingestor instance.
func NewIngestorV3(options ...IngestorOptionFuncV3) (*IngestorV3, error) {
	// Set up the ingestor
	i := &IngestorV3{
		clearExisting:        defaultClearExisting,
		compression:          defaultCompression,
		numActiveConnections: defaultNumActiveConnections,
		numWorkers:           defaultNumWorkers,
		numReplicas:          defaultNumReplicas,
		threshold:            defaultThreshold,
		bulkByteSize:         defaultBulkByteSize,
		scanBufferSize:       defaultScanBufferSize,
	}
	// Run the options through it
	for _, option := range options {
		if err := option(i); err != nil {
			return nil, err
		}
	}
	return i, nil
}

func (i *IngestorV3) prepareIndex() error {
	// check if index exists
	indexExists, err := i.client.IndexExists(i.index).Do()
	if err != nil {
		return err
	}
	// if index exists
	if indexExists && i.clearExisting {
		// send the delete index request
		log.Infof("Deleting existing index `%s`", i.index)
		res, err := i.client.DeleteIndex(i.index).Do()
		if err != nil {
			return fmt.Errorf("Error occured while deleting index: %v", err)
		}
		if !res.Acknowledged {
			return fmt.Errorf("Delete index request not acknowledged for index: `%s`", i.index)
		}
	}
	// instantiate a new document
	document, err := i.documentCtor()
	if err != nil {
		return err
	}
	// get the document mapping
	mapping, err := document.GetMapping()
	if err != nil {
		return err
	}
	// get the document type name
	typ, err := document.GetType()
	if err != nil {
		return err
	}
	// if index does not exist at this point, create it
	if !indexExists || i.clearExisting {
		// prepare the create index body
		body := fmt.Sprintf("{\"mappings\":%s,\"settings\":{\"number_of_replicas\":0}}", mapping)
		// send create index request
		log.Infof("Creating index `%s`", i.index)
		res, err := i.client.CreateIndex(i.index).Body(body).Do()
		if err != nil {
			return fmt.Errorf("Error occured while creating index: %v", err)
		}
		if !res.Acknowledged {
			return fmt.Errorf("Create index request not acknowledged for `%s`", i.index)
		}
	} else {
		// send put mapping request
		log.Infof("Putting mapping `%s`", i.index)
		res, err := i.client.PutMapping().Index(i.index).Type(typ).BodyString(mapping).Do()
		if err != nil {
			return fmt.Errorf("Error occured while updating mapping for index: %v", err)
		}
		if !res.Acknowledged {
			return fmt.Errorf("Put mapping request not acknowledged for `%s`", i.index)
		}
	}
	return nil
}

func (i *IngestorV3) enableReplicas() error {
	body := fmt.Sprintf("{\"index\":{\"number_of_replicas\":%d}}", i.numReplicas)
	log.Infof("Enabling replicas for index `%s`", i.index)
	res, err := i.client.IndexPutSettings(i.index).BodyString(body).Do()
	if err != nil {
		return fmt.Errorf("Error occurred while enabling replicas: %v", err)
	}
	if !res.Acknowledged {
		return fmt.Errorf("Enable replication index request not acknowledged for index `%s`", i.index)
	}
	return nil
}

// Ingest will run the ingest job.
func (i *IngestorV3) Ingest() error {

	// check that we have the required options set
	if i.index == "" {
		return fmt.Errorf("Ingestor index has not been set with SetIndex() option")
	}
	if i.documentCtor == nil {
		return fmt.Errorf("Ingestor document constructor has not been set with SetDocument() option")
	}
	if i.input == nil {
		return fmt.Errorf("Ingestor input has not been set with SetInput() option")
	}
	if i.client == nil {
		return fmt.Errorf("Ingestor Elasticsearch client has not been set with SetClient() option")
	}

	// print input summary
	log.Info(i.input.Summary())

	// prepare elasticsearch index
	err := i.prepareIndex()
	if err != nil {
		return err
	}

	// open the backpressure equalizer
	equalizer.Open(i.numActiveConnections)

	// create pool of size N
	p := pool.New(i.numWorkers)

	// start progress tracking
	progress.StartProgress()

	// launch the ingest job
	err = p.Execute(i.newlineWorker(), i.input)
	if err != nil {
		// error threshold was surpassed or there was a fatal error
		progress.EndProgress()
		progress.PrintFailure()
		return err
	}

	// success
	progress.EndProgress()
	progress.PrintSuccess()

	// close the backpressure equalizer
	equalizer.Close()

	// enable replication
	if i.numReplicas > 0 {
		err := i.enableReplicas()
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *IngestorV3) newBulkIndexRequest(line string) (*elastic.BulkIndexRequest, error) {
	// instantiate a new document
	document, err := i.documentCtor()
	if err != nil {
		return nil, err
	}
	// set data for document
	err = document.SetData(line)
	if err != nil {
		return nil, err
	}
	// get id from document
	id, err := document.GetID()
	if err != nil {
		return nil, err
	}
	// gracefully handle nil id
	if id == "" {
		return nil, nil
	}
	// get type from document
	typ, err := document.GetType()
	if err != nil {
		return nil, err
	}
	// gracefully handle nil type
	if typ == "" {
		return nil, nil
	}
	// get source from document
	source, err := document.GetSource()
	if err != nil {
		return nil, err
	}
	// gracefully handle nil source
	if source == nil {
		return nil, nil
	}
	// create index action
	return elastic.NewBulkIndexRequest().Id(id).Type(typ).Doc(source), nil
}

func (i *IngestorV3) newlineWorker() pool.Worker {
	return func(next io.Reader) error {

		// get decompress reader (if compression is specified / supported)
		reader, err := getReader(next, i.compression)
		if threshold.CheckErr(err, i.threshold) {
			return threshold.NewErr(i.threshold)
		}

		// scan file line by line
		scanner := bufio.NewScanner(reader)
		// allocate a large enough buffer
		scanner.Buffer(make([]byte, i.scanBufferSize), i.scanBufferSize)

		// total bytes sent
		bytes := int64(0)

		for {
			// create a new bulk request object
			bulk := equalizer.NewRequestV3(i.client, i.index)

			// begin reading file, line by line
			for scanner.Scan() {

				// read line of file
				line := scanner.Text()

				// create bulk index request
				req, err := i.newBulkIndexRequest(line)
				if threshold.CheckErr(err, i.threshold) {
					return threshold.NewErr(i.threshold)
				}

				// ensure that the request was created
				if req != nil {
					// add index action to bulk req
					bulk.Add(req)
					// flag this document as successful
					threshold.AddSuccess()
					// check if we have hit batch size limit
					if bulk.EstimatedSizeInBytes() >= i.bulkByteSize {
						// ready to send
						break
					}
				}
			}

			// check if scanner encountered an err
			err := scanner.Err()
			if threshold.CheckErr(err, i.threshold) {
				return threshold.NewErr(i.threshold)
			}

			// if no actions, we are finished
			if bulk.NumberOfActions() == 0 {
				break
			}

			// add total bytes
			bytes += int64(bulk.EstimatedSizeInBytes())

			// create the callback to be executed after this bulk request
			// succeeds. This is required ensure that the correct `bytes`
			// value is snapshotted.
			callback := func(bytes int64) equalizer.CallbackFunc {
				return func() {
					// update and print current progress
					progress.UpdateProgress(bytes)
				}
			}(bytes)

			// send the request through the equalizer, this will wait until the
			// equalizer determines ES is 'ready'.
			// NOTE: Due to the asynchronous nature of the equalizer, error
			// values returned here may not be caused from this worker
			// goroutine.
			err = equalizer.SendV3(bulk, callback)
			if err != nil {
				// always return on bulk ingest error
				return err
			}

		}
		return nil
	}
}
