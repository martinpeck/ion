package committer

import (
	"encoding/json"
	"fmt"
	"github.com/lawrencegripper/ion/internal/app/handler/development"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/lawrencegripper/ion/internal/app/handler/dataplane"
	"github.com/lawrencegripper/ion/internal/app/handler/dataplane/documentstorage"
	"github.com/lawrencegripper/ion/internal/app/handler/helpers"
	"github.com/lawrencegripper/ion/internal/app/handler/logger"
	"github.com/lawrencegripper/ion/internal/app/handler/module"
	"github.com/lawrencegripper/ion/internal/pkg/common"
)

// cSpell:ignore logrus, GUID, nolint

const (
	eventTypeKey      = "eventType"
	filesToIncludeKey = "files"
)

// Committer holds the data and methods needed to commit
// the module's environment to the data plane.
type Committer struct {
	dataPlane   *dataplane.DataPlane
	context     *common.Context
	environment *module.Environment

	executionID     string
	validEventTypes []string

	baseDir   string
	devConfig *development.Configuration
}

// NewCommitter creates a new committer instance
func NewCommitter(baseDir string, devCfg *development.Configuration) *Committer {
	if baseDir == "" {
		baseDir = "/ion/"
	}

	if devCfg == nil {
		devCfg = &development.Configuration{}
	}

	committer := &Committer{
		baseDir:   baseDir,
		devConfig: devCfg,
	}

	return committer
}

// Commit persists the module's environment in the data plane
func (c *Committer) Commit(context *common.Context, dataPlane *dataplane.DataPlane, validEventTypes []string) error {

	if err := helpers.ErrorIfNil(dataPlane, context); err != nil {
		return err
	}
	if err := helpers.ErrorIfNil(dataPlane.BlobStorageProvider, dataPlane.DocumentStorageProvider, dataPlane.EventPublisher); err != nil {
		return err
	}
	if err := helpers.ErrorIfEmpty(context.EventID); err != nil {
		return err
	}

	c.executionID = helpers.NewGUID()
	c.validEventTypes = validEventTypes
	c.dataPlane = dataPlane
	c.environment = module.GetModuleEnvironment(c.baseDir)
	c.context = context

	if err := c.doCommit(); err != nil {
		return err
	}

	return nil
}

// Close cleans up any external resources
func (c *Committer) Close() {
	logger.Info(c.context, "cleaning up handler")

	_ = c.environment.Clear()
	defer c.dataPlane.Close()
}

// Commit is called when the module is finished and wishes to commit their state to an external provider
func (c *Committer) doCommit() error {
	logger.Info(c.context, "committing module's environment to the data plane")

	// Commit blob data to an external blob store
	blobURIs, err := c.commitBlob(c.environment.OutputBlobDirPath)
	if err != nil {
		return fmt.Errorf("error committing blob data: %+v", err)
	}

	// Commit metadata to an external document store
	err = c.commitInsights(c.environment.OutputMetaFilePath)
	if err != nil {
		return fmt.Errorf("error committing meta data: %+v", err)
	}

	// Commit events to an external messaging system
	err = c.commitEvents(c.environment.OutputEventsDirPath, blobURIs)
	if err != nil {
		return fmt.Errorf("error committing events: %+v", err)
	}

	// If developmentFlag enabled, dump out an empty
	// file to indicate environment committed.
	if c.devConfig.Enabled {
		var empty struct{}
		_ = c.devConfig.WriteOutput("committed", empty)
	}

	logger.Info(c.context, "successfully committed module's environment to the data plane")
	return nil
}

//CommitBlob commits the blob directory to an external blob provider
func (c *Committer) commitBlob(blobsDir string) (map[string]string, error) {
	if _, err := os.Stat(blobsDir); os.IsNotExist(err) {
		logger.Debug(c.context, fmt.Sprintf("blob output directory '%s' does not exists '%+v'", blobsDir, err))
		return nil, nil
	}

	files := make([]string, 0)
	err := filepath.Walk(blobsDir, func(path string, f os.FileInfo, err error) error {
		if f.IsDir() {
			return nil
		}
		files = append(files, path)
		return err
	})
	if err != nil {
		return nil, err
	}

	blobURIs, err := c.dataPlane.PutBlobs(files)
	if err != nil {
		return nil, fmt.Errorf("failed to commit blob: %+v", err)
	}

	logger.Info(c.context, "committed blob data")
	logger.DebugWithFields(c.context, "blob file names", map[string]interface{}{
		"files": files,
	})
	return blobURIs, nil
}

//CommitMeta commits the metadata document to an external provider
func (c *Committer) commitInsights(insightsPath string) error {
	if _, err := os.Stat(insightsPath); os.IsNotExist(err) {
		logger.Info(c.context, fmt.Sprintf("insights file '%s' does not exists '%+v'", insightsPath, err))
		return nil
	}

	bytes, err := ioutil.ReadFile(insightsPath)
	if err != nil {
		return fmt.Errorf("failed to read insights document '%s' with error '%+v'", insightsPath, err)
	}
	if len(bytes) == 0 {
		return nil // Handle no insights
	}
	var m common.KeyValuePairs
	err = json.Unmarshal(bytes, &m)
	if err != nil {
		return fmt.Errorf("failed to unmarshal insights '%s' with error: '%+v'", insightsPath, err)
	}
	insight := documentstorage.Insight{
		Context:     c.context,
		ExecutionID: c.executionID,
		Data:        m,
	}
	err = c.dataPlane.CreateInsight(&insight)
	if err != nil {
		return fmt.Errorf("failed to add insights document '%+v' with error: '%+v'", m, err)
	}

	if c.devConfig.Enabled {
		_ = c.devConfig.WriteInsight("insights.json", insight)
	}

	logger.Info(c.context, "committed insights data")
	logger.DebugWithFields(c.context, "insights data", map[string]interface{}{
		"insight": insight,
	})
	return nil
}

//CommitEvents commits the events directory to an external provider
func (c *Committer) commitEvents(eventsPath string, blobURIs map[string]string) error {
	if _, err := os.Stat(eventsPath); os.IsNotExist(err) {
		logger.Info(c.context, fmt.Sprintf("events output directory '%s' does not exists '%+v'", eventsPath, err))
		return nil
	}

	// Read each of the event files stored in the
	// output events directory. Events will be
	// de-serialized into an expected structure,
	// enriched, validated and then split into
	// an event to send via the messaging system
	// and a context document for the event to
	// reference.
	files, err := ioutil.ReadDir(eventsPath)
	if err != nil {
		return err
	}
	for _, file := range files {
		fileName := file.Name()
		eventFilePath := path.Join(eventsPath, fileName)
		f, err := os.Open(eventFilePath)
		defer f.Close() // nolint: errcheck
		if err != nil {
			return fmt.Errorf("failed to read file '%s' with error: '%+v'", fileName, err)
		}

		// Decode event into map
		var keyValuePairs common.KeyValuePairs
		decoder := json.NewDecoder(f)
		err = decoder.Decode(&keyValuePairs)
		if err != nil {
			return fmt.Errorf("failed to unmarshal map '%s' with error: '%+v'", fileName, err)
		}
		logger.DebugWithFields(c.context, "event data", map[string]interface{}{
			"event": keyValuePairs,
		})

		var eventType string
		var includedFilesCSV string
		var eventTypeIndex, filesIndex int

		// For each key/value in event data array.
		for i, kvp := range keyValuePairs {
			// Check the key against required keys
			switch kvp.Key {
			case eventTypeKey:
				// Check whether the event type is valid for this module
				if helpers.ContainsString(c.validEventTypes, kvp.Value) == false {
					logger.Info(c.context, fmt.Sprintf("this module is unable to publish event's of type '%s'", eventType))
					continue
				}
				eventType = kvp.Value
				eventTypeIndex = i
				break
			case filesToIncludeKey:
				includedFilesCSV = kvp.Value
				filesIndex = i
				break
			default:
				// Ignore non required keys
				break
			}
		}
		itemsRemoved := 0

		// [Required] Check that the key 'eventType' was found in the data
		// if it wasn't return an error. If it was, remove it
		// from the key value pairs as it is no longer needed
		if eventType == "" {
			return fmt.Errorf("all events must contain an 'eventType' field, error: '%+v'", err)
		}
		keyValuePairs, err = keyValuePairs.Remove(eventTypeIndex)
		if err != nil {
			return fmt.Errorf("error removing event type from metadata: '%+v'", err)
		}
		itemsRemoved++

		// [Optional] Check whether the key 'files' was supplied in order
		// to pass file references to event context. If it wasn't, log it
		// and ignore it. If it was, remove it from the key value pairs
		// as it is no longer needed and then add the file list and their
		// blob uri for each of the files to the event context.
		var fileSlice []string
		if len(includedFilesCSV) == 0 {
			logger.Info(c.context, "event contains no file references")
		} else {
			keyValuePairs, err = keyValuePairs.Remove(filesIndex - itemsRemoved)
			if err != nil {
				return fmt.Errorf("error removing event type from metadata: '%+v'", err)
			}
			itemsRemoved++
			fileSlice = strings.Split(includedFilesCSV, ",")
			for _, f := range fileSlice {
				blobInfo := common.KeyValuePair{
					Key:   f,
					Value: blobURIs[f],
				}
				keyValuePairs = keyValuePairs.Append(blobInfo)
			}
		}

		eventID := helpers.NewGUID()

		// Create a new context for this event.
		// We can only build a partial context
		// as we don't know which modules will
		// process the message.
		// The context will be completed later.
		context := &common.Context{
			CorrelationID: c.context.CorrelationID,
			ParentEventID: c.context.EventID,
			EventID:       eventID,
			Name:          c.context.Name,
		}

		// Create a new event to publish
		// via the messaging system.
		// This will embed the context
		// created above.
		event := common.Event{
			Context: context,
			Type:    eventType,
		}

		// Create event metadata that
		// can store additional metadata
		// without bloating th event such
		// as a list of files to process.
		// This will be looked up by
		// the processing modules using the
		// event id.
		eventMeta := documentstorage.EventMeta{
			Context: context,
			Files:   fileSlice,
			Data:    keyValuePairs,
		}
		err = c.dataPlane.CreateEventMeta(&eventMeta)
		if err != nil {
			return fmt.Errorf("failed to add context '%+v' with error '%+v'", eventMeta, err)
		}
		err = c.dataPlane.Publish(event)
		if err != nil {
			return fmt.Errorf("failed to publish event '%+v' with error '%+v'", event, err)
		}
		if c.devConfig.Enabled {
			_ = c.devConfig.WriteMetadata(fileName, eventMeta)
			_ = c.devConfig.WriteEvent(fileName, event)
		}
	}

	logger.Info(c.context, "committed events")
	return nil
}
