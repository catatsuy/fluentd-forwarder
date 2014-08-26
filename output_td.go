package fluentd_forwarder

import (
	"bytes"
	"github.com/ugorji/go/codec"
	td_client "github.com/treasure-data/td-client-go"
	logging "github.com/op/go-logging"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
	"os"
	"unsafe"
	"errors"
	"compress/gzip"
	"strings"
)

type tdOutputSpooler struct {
	daemon       *tdOutputSpoolerDaemon
	ticker       *time.Ticker
	tag          string
	databaseName string
	tableName    string
	key          string
	journal      Journal
	client       *td_client.TDClient
	shutdownChan chan struct{}
	isShuttingDown    unsafe.Pointer
}

type tdOutputSpoolerDaemon struct {
	output       *TDOutput
	shutdownChan chan struct{}
	spoolersMtx  sync.Mutex
	spoolers     map[string]*tdOutputSpooler
	tempFactory  TempFileRandomAccessStoreFactory
	wg           sync.WaitGroup
}

type TDOutput struct {
	logger            *logging.Logger
	codec             *codec.MsgpackHandle
	retryInterval     time.Duration
	databaseName      string
	tableName         string
	tempDir           string
	enc               *codec.Encoder
	conn              net.Conn
	flushInterval     time.Duration
	wg                sync.WaitGroup
	journalGroup      JournalGroup
	emitterChan       chan FluentRecordSet
	spoolerDaemon     *tdOutputSpoolerDaemon
	isShuttingDown    unsafe.Pointer
	client            *td_client.TDClient
}

func encodeRecords(encoder *codec.Encoder, records []TinyFluentRecord) error {
	for _, record := range records {
		e := map[string]interface{}{ "time": record.Timestamp }
		for k, v := range record.Data {
			e[k] = v
		}
		err := encoder.Encode(e)
		if err != nil {
			return err
		}
	}
	return nil
}

func (spooler *tdOutputSpooler) cleanup() {
	spooler.ticker.Stop()
	spooler.journal.Dispose()
	spooler.daemon.wg.Done()
}

func (spooler *tdOutputSpooler) handle() {
	defer spooler.cleanup()
	spooler.daemon.output.logger.Notice("Spooler started")
	outer: for {
		select {
		case <-spooler.ticker.C:
			spooler.daemon.output.logger.Notice("Flushing...")
			err := spooler.journal.Flush(func(chunk JournalChunk) error {
				defer chunk.Dispose()
				spooler.daemon.output.logger.Info("Flushing chunk %s", chunk.String())
				_, err := spooler.client.Import(
					spooler.databaseName,
					spooler.tableName,
					"msgpack.gz",
					NewCompressingBlob(
						chunk,
						16777216,
						gzip.BestSpeed,
						&spooler.daemon.tempFactory,
					),
					chunk.Id(),
				)
				return err
			})
			if err != nil {
				spooler.daemon.output.logger.Error("Error during reading from the journal: %s", err.Error())
			}
		case <-spooler.shutdownChan:
			break outer
		}
	}
	spooler.daemon.output.logger.Notice("Spooler ended")
}

func normalizeDatabaseName(name string) (string, error) {
	name_ := ([]byte)(name)
	if len(name_) == 0 {
		return "", errors.New("Empty name is not allowed")
	}
	if len(name_) < 3 {
		name_ = append(name_, ("___"[0:3 - len(name)])...)
	}
	if 255 < len(name_) {
		name_ = append(name_[0:253], "__"...)
	}
	name_ = bytes.ToLower(name_)
	for i, c := range name_ {
		if !((c >= 'a' || c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
			c = '_'
		}
		name_[i] = c
	}
	return (string)(name_), nil
}

func normalizeTableName(name string) (string, error) {
	return normalizeDatabaseName(name)
}

func newTDOutputSpooler(daemon *tdOutputSpoolerDaemon, databaseName, tableName, key string) *tdOutputSpooler {
	journal := daemon.output.journalGroup.GetJournal(key)
	return &tdOutputSpooler {
		daemon: daemon,
		ticker: time.NewTicker(daemon.output.flushInterval),
		databaseName: databaseName,
		tableName: tableName,
		key: key,
		journal: journal,
		shutdownChan: make(chan struct{}, 1),
		client: daemon.output.client,
	}
}

func (daemon *tdOutputSpoolerDaemon) spawnSpooler(databaseName, tableName, key string) *tdOutputSpooler {
	daemon.spoolersMtx.Lock()
	defer daemon.spoolersMtx.Unlock()
	spooler, exists := daemon.spoolers[key]
	if exists {
		return spooler
	}
	spooler = newTDOutputSpooler(daemon, databaseName, tableName, key)
	daemon.output.logger.Notice("Spawning spooler " + spooler.key)
	daemon.spoolers[spooler.key] = spooler
	daemon.wg.Add(1)
	go spooler.handle()
	return spooler
}

func (daemon *tdOutputSpoolerDaemon) cleanup() {
	func() {
		daemon.spoolersMtx.Lock()
		defer daemon.spoolersMtx.Unlock()
		for _, spooler := range daemon.spoolers {
			if atomic.CompareAndSwapPointer(&spooler.isShuttingDown, unsafe.Pointer(uintptr(0)), unsafe.Pointer(uintptr(1))) {
				spooler.shutdownChan <- struct{}{}
			}
		}
	}()
	daemon.wg.Wait()
	daemon.output.wg.Done()
}

func (daemon *tdOutputSpoolerDaemon) handle() {
	defer daemon.cleanup();
	daemon.output.logger.Notice("Spooler daemon started")
	outer: for {
		select {
		case <-daemon.shutdownChan:
			break outer
		}
	}
	daemon.output.logger.Notice("Spooler ended")
}

func newTDOutputSpoolerDaemon(output *TDOutput) *tdOutputSpoolerDaemon {
	return &tdOutputSpoolerDaemon {
		output: output,
		shutdownChan: make(chan struct{}, 1),
		spoolers: make(map[string]*tdOutputSpooler),
		tempFactory: TempFileRandomAccessStoreFactory { output.tempDir, "", },
		wg: sync.WaitGroup {},
	}
}

func (output *TDOutput) spawnSpoolerDaemon() {
	output.logger.Notice("Spawning spooler daemon")
	output.spoolerDaemon = newTDOutputSpoolerDaemon(output)
	output.wg.Add(1)
	go output.spoolerDaemon.handle()
}


func (daemon *tdOutputSpoolerDaemon) getSpooler(tag string) (*tdOutputSpooler, error) {
	databaseName := daemon.output.databaseName
	tableName := daemon.output.tableName
	if databaseName == "*" {
		if tableName == "*" {
			c := strings.SplitN(tag, ".", 2)
			if len(c) == 1 {
				tableName = c[0]
			} else if len(c) == 2 {
				databaseName = c[0]
				tableName = c[1]
			}
		} else {
			databaseName = tag
		}
	} else {
		if tableName == "*" {
			tableName = tag
		}
	}
	databaseName, err := normalizeDatabaseName(databaseName)
	if err != nil {
		return nil, err
	}
	tableName, err = normalizeTableName(tableName)
	if err != nil {
		return nil, err
	}
	key := databaseName + "." + tableName
	return daemon.spawnSpooler(databaseName, tableName, key), nil
}

func (output *TDOutput) spawnEmitter() {
	output.logger.Notice("Spawning emitter")
	output.wg.Add(1)
	go func() {
		defer func() {
			output.spoolerDaemon.shutdownChan <- struct{}{}
			output.wg.Done()
		}()
		output.logger.Notice("Emitter started")
		buffer := bytes.Buffer{}
		for recordSet := range output.emitterChan {
			buffer.Reset()
			encoder := codec.NewEncoder(&buffer, output.codec)
			err := func() error {
				spooler, err := output.spoolerDaemon.getSpooler(recordSet.Tag)
				if err != nil {
					return err
				}
				err = encodeRecords(encoder, recordSet.Records)
				if err != nil {
					return err
				}
				output.logger.Debug("Emitter processed %d entries", len(recordSet.Records))
				return spooler.journal.Write(buffer.Bytes())
			}()
			if err != nil {
				output.logger.Error("%s", err.Error())
				continue
			}
		}
		output.logger.Notice("Emitter ended")
	}()
}

func (output *TDOutput) Emit(recordSets []FluentRecordSet) error {
	defer func() {
		recover()
	}()
	for _, recordSet := range recordSets {
		output.emitterChan <- recordSet
	}
	return nil
}

func (output *TDOutput) String() string {
	return "output"
}

func (output *TDOutput) Stop() {
	if atomic.CompareAndSwapPointer(&output.isShuttingDown, unsafe.Pointer(uintptr(0)), unsafe.Pointer(uintptr(1))) {
		close(output.emitterChan)
	}
}

func (output *TDOutput) WaitForShutdown() {
	output.wg.Wait()
}

func (output *TDOutput) Start() {
	output.spawnEmitter()
	output.spawnSpoolerDaemon()
}

func NewTDOutput(
	logger *logging.Logger,
	endpoint string,
	retryInterval time.Duration,
	connectionTimeout time.Duration,
	writeTimeout time.Duration,
	flushInterval time.Duration,
	journalGroupPath string,
	maxJournalChunkSize int64,
	apiKey string,
	databaseName string,
	tableName string,
	tempDir string,
	useSsl bool,
	httpProxy string,
) (*TDOutput, error) {
	_codec := codec.MsgpackHandle{}
	_codec.MapType = reflect.TypeOf(map[string]interface{}(nil))
	_codec.RawToString = false
	_codec.StructToArray = true

	journalFactory := NewFileJournalGroupFactory(
		logger,
		randSource,
		time.Now,
		".log",
		os.FileMode(0600),
		maxJournalChunkSize,
	)
	router := (td_client.EndpointRouter)(nil)
	if endpoint != "" {
		router = &td_client.FixedEndpointRouter { endpoint }
	}
	httpProxy_ := (interface{})(nil)
	if httpProxy != "" {
		httpProxy_ = httpProxy
	}
	client, err := td_client.NewTDClient(td_client.Settings {
		ApiKey: apiKey,
		Router: router,
		ConnectionTimeout: connectionTimeout,
		// ReadTimeout: readTimeout, // TODO
		SendTimeout: writeTimeout,
		Ssl: useSsl,
		Proxy: httpProxy_,
	})
	if err != nil {
		return nil, err
	}
	output := &TDOutput{
		logger:            logger,
		codec:             &_codec,
		retryInterval:     retryInterval,
		wg:                sync.WaitGroup{},
		flushInterval:     flushInterval,
		emitterChan:       make(chan FluentRecordSet),
		isShuttingDown:    unsafe.Pointer(uintptr(0)),
		client:            client,
		databaseName:      databaseName,
		tableName:         tableName,
		tempDir:           tempDir,
	}
	journalGroup, err := journalFactory.GetJournalGroup(journalGroupPath, output)
	if err != nil {
		return nil, err
	}
	defer func () {
		err := journalGroup.Dispose()
		if err != nil {
			logger.Error("%#v", err)
		}
	}()
	output.journalGroup  = journalGroup
	return output, nil
}