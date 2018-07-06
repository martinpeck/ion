package mongodb

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/lawrencegripper/ion/internal/app/handler/dataplane/documentstorage"
	"github.com/lawrencegripper/ion/internal/pkg/common"
	mongo "gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

// cSpell:ignore mongodb, bson, upsert

//Config used to setup a MongoDB metastore provider
type Config struct {
	Enabled    bool   `description:"Enable MongoDB metadata provider"`
	Name       string `description:"MongoDB database name"`
	Password   string `description:"MongoDB database password"`
	Collection string `description:"MongoDB database collection to use"`
	Port       int    `description:"MongoDB server port"`
}

//MongoDB handles the connection to an external Mongo database
type MongoDB struct {
	Session    *mongo.Session
	Collection *mongo.Collection
}

//NewMongoDB creates a new MongoDB object
func NewMongoDB(config *Config) (*MongoDB, error) {
	dialInfo := &mongo.DialInfo{
		Addrs:    []string{fmt.Sprintf("%s.documents.azure.com:%d", config.Name, config.Port)},
		Timeout:  10 * time.Second,
		Database: config.Name,
		Username: config.Name,
		Password: config.Password,
		DialServer: func(addr *mongo.ServerAddr) (net.Conn, error) {
			return tls.Dial("tcp", addr.String(), &tls.Config{})
		},
	}

	session, err := mongo.DialWithInfo(dialInfo)
	if err != nil {
		return nil, fmt.Errorf("can't connect to mongo, go error %v", err)
	}

	session.SetSafe(&mongo.Safe{})

	col := session.DB(config.Name).C(config.Collection)

	MongoDB := &MongoDB{
		Session:    session,
		Collection: col,
	}

	return MongoDB, nil
}

//GetEventMetaByID returns a single document matching a given document ID
func (db *MongoDB) GetEventMetaByID(id string) (*documentstorage.EventMeta, error) {
	eventMeta := documentstorage.EventMeta{}
	err := db.Collection.Find(bson.M{"id": id}).One(&eventMeta)
	if err != nil {
		return nil, fmt.Errorf("failed to get document with ID %s, error: %+v", id, err)
	}
	return &eventMeta, nil
}

//GetJSONDataByCorrelationID returns all documents associated with the correlationid
// returns the json from the resulting
func (db *MongoDB) GetJSONDataByCorrelationID(id string) (*string, error) {
	result := bson.Raw{}
	err := db.Collection.Find(bson.M{"context.correlationId": id}).All(&result)
	if err != nil {
		return nil, fmt.Errorf("failed to get document with ID %s, error: %+v", id, err)
	}

	var i interface{}
	if err = result.Unmarshal(&i); err != nil {
		return nil, fmt.Errorf("failed to deserialize bson results for correlationID %s, error: %+v", id, err)
	}
	data, err := json.Marshal(i)
	if err != nil {
		return nil, fmt.Errorf("failed to get json for results for correlationID %s, error: %+v", id, err)
	}
	json := string(data)

	return &json, nil
}

//CreateEventMeta creates a new event context document
func (db *MongoDB) CreateEventMeta(eventMeta *documentstorage.EventMeta) error {
	eventMeta.Context.DocumentType = common.EventMetaDocType
	b, err := JSONMarshal(*eventMeta)
	if err != nil {
		return fmt.Errorf("error serializing JSON document: %+v", err)
	}
	var bsonDocument interface{}
	err = bson.UnmarshalJSON(b, &bsonDocument)
	if err != nil {
		return fmt.Errorf("error de-serializing to BSON: %+v", err)
	}
	selector := bson.M{"id": eventMeta.EventID}
	update := bson.M{"$set": eventMeta}
	_, err = db.Collection.Upsert(selector, update)
	if err != nil {
		return fmt.Errorf("error creates document: %+v", err)
	}
	return nil
}

//CreateInsight creates an insights document
func (db *MongoDB) CreateInsight(insight *documentstorage.Insight) error {
	insight.Context.DocumentType = common.InsightDocType
	b, err := JSONMarshal(*insight)
	if err != nil {
		return fmt.Errorf("error serializing JSON document: %+v", err)
	}
	var bsonDocument interface{}
	err = bson.UnmarshalJSON(b, &bsonDocument)
	if err != nil {
		return fmt.Errorf("error de-serializing into BSON: %+v", err)
	}
	selector := bson.M{"id": insight.ExecutionID}
	update := bson.M{"$set": insight}
	_, err = db.Collection.Upsert(selector, update)
	if err != nil {
		return fmt.Errorf("error creates document: %+v", err)
	}
	return nil
}

//Close cleans up the connection to Mongo
func (db *MongoDB) Close() {
	defer db.Session.Close()
}

//JSONMarshal encode to json without escaping html
func JSONMarshal(t interface{}) ([]byte, error) {
	buffer := &bytes.Buffer{}
	encoder := json.NewEncoder(buffer)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(t)
	return buffer.Bytes(), err
}
