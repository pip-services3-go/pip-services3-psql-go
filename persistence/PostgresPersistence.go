package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgproto3/v2"
	"github.com/jackc/pgx/v4/pgxpool"
	cconf "github.com/pip-services3-go/pip-services3-commons-go/config"
	cconv "github.com/pip-services3-go/pip-services3-commons-go/convert"
	cdata "github.com/pip-services3-go/pip-services3-commons-go/data"
	cerr "github.com/pip-services3-go/pip-services3-commons-go/errors"
	cref "github.com/pip-services3-go/pip-services3-commons-go/refer"
	clog "github.com/pip-services3-go/pip-services3-components-go/log"
	cmpersist "github.com/pip-services3-go/pip-services3-data-go/persistence"
)

/*
Abstract persistence component that stores data in PostgreSQL using plain driver.

This is the most basic persistence component that is only
able to store data items of any type. Specific CRUD operations
over the data items must be implemented in child classes by
accessing c._db or c._collection properties.

### Configuration parameters ###

- collection:                  (optional) PostgreSQL collection name
- connection(s):
  - discovery_key:             (optional) a key to retrieve the connection from [[https://rawgit.com/pip-services-node/pip-services3-components-node/master/doc/api/interfaces/connect.idiscovery.html IDiscovery]]
  - host:                      host name or IP address
  - port:                      port number (default: 27017)
  - uri:                       resource URI or connection string with all parameters in it
- credential(s):
  - store_key:                 (optional) a key to retrieve the credentials from [[https://rawgit.com/pip-services-node/pip-services3-components-node/master/doc/api/interfaces/auth.icredentialstore.html ICredentialStore]]
  - username:                  (optional) user name
  - password:                  (optional) user password
- options:
  - connect_timeout:      (optional) number of milliseconds to wait before timing out when connecting a new client (default: 0)
  - idle_timeout:         (optional) number of milliseconds a client must sit idle in the pool and not be checked out (default: 10000)
  - max_pool_size:        (optional) maximum number of clients the pool should contain (default: 10)

### References ###

- \*:logger:\*:\*:1.0           (optional) [[https://rawgit.com/pip-services-node/pip-services3-components-node/master/doc/api/interfaces/log.ilogger.html ILogger]] components to pass log messages
- \*:discovery:\*:\*:1.0        (optional) [[https://rawgit.com/pip-services-node/pip-services3-components-node/master/doc/api/interfaces/connect.idiscovery.html IDiscovery]] services
- \*:credential-store:\*:\*:1.0 (optional) Credential stores to resolve credentials

### Example ###

    class MyPostgresPersistence extends PostgresPersistence<MyData> {

      func (c * PostgresPersistence) constructor() {
          base("mydata");
      }

      func (c * PostgresPersistence) getByName(correlationId: string, name: string, callback: (err, item) => void) {
        let criteria = { name: name };
        c._model.findOne(criteria, callback);
      });

      func (c * PostgresPersistence) set(correlatonId: string, item: MyData, callback: (err) => void) {
        let criteria = { name: item.name };
        let options = { upsert: true, new: true };
        c._model.findOneAndUpdate(criteria, item, options, callback);
      }

    }

    let persistence = new MyPostgresPersistence();
    persistence.configure(ConfigParams.fromTuples(
        "host", "localhost",
        "port", 27017
    ));

    persitence.open("123", (err) => {
         ...
    });

    persistence.set("123", { name: "ABC" }, (err) => {
        persistence.getByName("123", "ABC", (err, item) => {
            console.log(item);                   // Result: { name: "ABC" }
        });
    });
*/

type PostgresPersistence struct {
	defaultConfig *cconf.ConfigParams

	config          *cconf.ConfigParams
	references      cref.IReferences
	opened          bool
	localConnection bool
	autoObjects     []string

	//The dependency resolver.
	DependencyResolver *cref.DependencyResolver
	//The logger.
	Logger *clog.CompositeLogger
	//The PostgreSQL connection component.
	Connection *PostgresConnection
	//The PostgreSQL connection pool object.
	Client *pgxpool.Pool
	//The PostgreSQL database name.
	DatabaseName string
	//The PostgreSQL table object.
	TableName   string
	MaxPageSize int
	Prototype   reflect.Type
}

// Creates a new instance of the persistence component.
// - tableName    (optional) a table name.
func NewPostgresPersistence(proto reflect.Type, tableName string) *PostgresPersistence {
	c := &PostgresPersistence{
		defaultConfig: cconf.NewConfigParamsFromTuples(
			"collection", nil,
			"dependencies.connection", "*:connection:postgres:*:1.0",
			"options.max_pool_size", 2,
			"options.keep_alive", 1,
			"options.connect_timeout", 5000,
			"options.auto_reconnect", true,
			"options.max_page_size", 100,
			"options.debug", true,
		),
		autoObjects: make([]string, 0),
		Logger:      clog.NewCompositeLogger(),
		MaxPageSize: 100,
		Prototype:   proto,
	}
	c.DependencyResolver = cref.NewDependencyResolver()
	c.DependencyResolver.Configure(c.defaultConfig)
	c.TableName = tableName
	return c
}

// Configures component by passing configuration parameters.
// - config    configuration parameters to be set.
func (c *PostgresPersistence) Configure(config *cconf.ConfigParams) {
	config = config.SetDefaults(c.defaultConfig)
	c.config = config

	c.DependencyResolver.Configure(config)

	c.TableName = config.GetAsStringWithDefault("collection", c.TableName)
	c.TableName = config.GetAsStringWithDefault("table", c.TableName)
	c.MaxPageSize = config.GetAsIntegerWithDefault("options.max_page_size", c.MaxPageSize)
}

// Sets references to dependent components.
// - references 	references to locate the component dependencies.
func (c *PostgresPersistence) SetReferences(references cref.IReferences) {
	c.references = references
	c.Logger.SetReferences(references)

	// Get connection
	c.DependencyResolver.SetReferences(references)
	result := c.DependencyResolver.GetOneOptional("connection")
	if dep, ok := result.(*PostgresConnection); ok {
		c.Connection = dep
	}
	// Or create a local one
	if c.Connection == nil {
		c.Connection = c.createConnection()
		c.localConnection = true
	} else {
		c.localConnection = false
	}
}

// Unsets (clears) previously set references to dependent components.
func (c *PostgresPersistence) UnsetReferences() {
	c.Connection = nil
}

func (c *PostgresPersistence) createConnection() *PostgresConnection {
	connection := NewPostgresConnection()
	if c.config != nil {
		connection.Configure(c.config)
	}
	if c.references != nil {
		connection.SetReferences(c.references)
	}
	return connection
}

// Adds index definition to create it on opening
// - keys index keys (fields)
// - options index options
func (c *PostgresPersistence) EnsureIndex(name string, keys map[string]string, options map[string]string) {
	builder := "CREATE"
	if options == nil {
		options = make(map[string]string, 0)
	}

	if options["unique"] != "" {
		builder += " UNIQUE"
	}

	builder += " INDEX IF NOT EXISTS " + name + " ON " + c.QuoteIdentifier(c.TableName)

	if options["type"] != "" {
		builder += " " + options["type"]
	}

	fields := ""
	for key, _ := range keys {
		if fields != "" {
			fields += ", "
		}
		fields += c.QuoteIdentifier(key)
		asc := keys[key]
		if asc != "" {
			fields += " DESC"
		}
	}

	builder += "(" + fields + ")"

	c.AutoCreateObject(builder)
}

// Adds index definition to create it on opening
// - dmlStatement DML statement to autocreate database object
func (c *PostgresPersistence) AutoCreateObject(dmlStatement string) {
	c.autoObjects = append(c.autoObjects, dmlStatement)
}

// Converts object value from internal to func (c * PostgresPersistence) format.
// - value     an object in internal format to convert.
// Returns converted object in func (c * PostgresPersistence) format.
func (c *PostgresPersistence) ConvertToPublic(value interface{}) interface{} {
	return value
}

// Convert object value from func (c * PostgresPersistence) to internal format.
// - value     an object in func (c * PostgresPersistence) format to convert.
// Returns converted object in internal format.
func (c *PostgresPersistence) ConvertFromPublic(value interface{}) interface{} {
	return value
}

func (c *PostgresPersistence) QuoteIdentifier(value string) string {
	if value == "" {
		return value
	}
	if value[0] == '\'' {
		return value
	}
	return "\"" + value + "\""
}

// Checks if the component is opened.
// Returns true if the component has been opened and false otherwise.
func (c *PostgresPersistence) IsOpen() bool {
	return c.opened
}

// Opens the component.
// - correlationId 	(optional) transaction id to trace execution through call chain.
// - Returns 			 error or nil no errors occured.
func (c *PostgresPersistence) Open(correlationId string) (err error) {
	if c.opened {
		return nil
	}

	if c.Connection == nil {
		c.Connection = c.createConnection()
		c.localConnection = true
	}

	if c.localConnection {
		err = c.Connection.Open(correlationId)
	}

	if err == nil && c.Connection == nil {
		err = cerr.NewInvalidStateError(correlationId, "NO_CONNECTION", "PostgreSQL connection is missing")
	}

	if err == nil && !c.Connection.IsOpen() {
		err = cerr.NewConnectionError(correlationId, "CONNECT_FAILED", "PostgreSQL connection is not opened")
	}

	c.opened = false

	if err != nil {
		return err
	}
	c.Client = c.Connection.GetConnection()
	c.DatabaseName = c.Connection.GetDatabaseName()

	// Recreate objects
	err = c.AutoCreateObjects(correlationId)
	if err != nil {
		c.Client = nil
		err = cerr.NewConnectionError(correlationId, "CONNECT_FAILED", "Connection to postgres failed").WithCause(err)
	} else {
		c.opened = true
		c.Logger.Debug(correlationId, "Connected to postgres database %s, collection %s", c.DatabaseName, c.QuoteIdentifier(c.TableName))
	}

	return err

}

// Closes component and frees used resources.
// - correlationId 	(optional) transaction id to trace execution through call chain.
// - Returns 			error or nil no errors occured.
func (c *PostgresPersistence) Close(correlationId string) (err error) {
	if !c.opened {
		return nil
	}

	if c.Connection == nil {
		return cerr.NewInvalidStateError(correlationId, "NO_CONNECTION", "Postgres connection is missing")
	}

	if c.localConnection {
		err = c.Connection.Close(correlationId)
	}
	if err != nil {
		return err
	}
	c.opened = false
	c.Client = nil
	return nil
}

// Clears component state.
// - correlationId 	(optional) transaction id to trace execution through call chain.
// - Returns 			error or nil no errors occured.
func (c *PostgresPersistence) Clear(correlationId string) error {
	// Return error if collection is not set
	if c.TableName == "" {
		return errors.New("Table name is not defined")
	}

	query := "DELETE FROM " + c.QuoteIdentifier(c.TableName)

	_, err := c.Client.Query(context.TODO(), query)
	if err != nil {
		err = cerr.NewConnectionError(correlationId, "CONNECT_FAILED", "Connection to postgres failed").
			WithCause(err)
	}
	return err
}

func (c *PostgresPersistence) AutoCreateObjects(correlationId string) (err error) {
	if c.autoObjects == nil || len(c.autoObjects) == 0 {
		return nil
	}

	// Check if table exist to determine weither to auto create objects
	query := "SELECT to_regclass('" + c.TableName + "')"
	result, qErr := c.Client.Query(context.TODO(), query)
	if qErr != nil {
		return qErr
	}

	// If table already exists then exit
	if result != nil && result.Next() {
		val, cErr := result.Values()
		if cErr != nil {
			return cErr
		}

		if len(val) > 0 && val[0] == c.TableName {
			return nil
		}
	}
	c.Logger.Debug(correlationId, "Table "+c.TableName+" does not exist. Creating database objects...")
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, dml := range c.autoObjects {
			_, err := c.Client.Query(context.TODO(), dml)
			if err != nil {
				c.Logger.Error(correlationId, err, "Failed to autocreate database object")
			}
		}
	}()
	wg.Wait()
	return nil
}

// Generates a list of column names to use in SQL statements like: "column1,column2,column3"
// - values an array with column values or a key-value map
// Returns a generated list of column names
func (c *PostgresPersistence) GenerateColumns(values interface{}) string {

	result := strings.Builder{}
	// String arrays
	if val, ok := values.([]string); ok {
		for _, item := range val {
			if result.String() != "" {
				result.WriteString(",")
			}
			result.WriteString(c.QuoteIdentifier(item))
		}
		return result.String()
	}

	if reflect.TypeOf(values).Kind() == reflect.Array {
		panic("Values must be string array")
	}

	if val, ok := values.(map[string]interface{}); ok {
		for item, _ := range val {
			if result.String() != "" {
				result.WriteString(",")
			}
			result.WriteString(c.QuoteIdentifier(item))
		}
		return result.String()
	}

	if reflect.TypeOf(values).Kind() == reflect.Map {
		panic("Values must be map[string]interface{}")
	}

	object := reflect.ValueOf(values)
	if object.Kind() == reflect.Ptr {
		object = object.Elem()
	}
	typ := object.Type()
	for i := 0; i < object.NumField(); i++ {
		if result.String() != "" {
			result.WriteString(",")
		}
		result.WriteString(c.QuoteIdentifier(typ.Field(i).Name))
	}
	return result.String()
}

// Generates a list of value parameters to use in SQL statements like: "$1,$2,$3"
// - values an array with values or a key-value map
// Returns a generated list of value parameters
func (c *PostgresPersistence) GenerateParameters(values interface{}) string {

	// String arrays
	if val, ok := values.([]interface{}); ok {
		return c.generateParamString(len(val))
	}

	if reflect.TypeOf(values).Kind() == reflect.Array {
		panic("Values must be string array")
	}

	if val, ok := values.(map[string]interface{}); ok {
		return c.generateParamString(len(val))
	}

	if reflect.TypeOf(values).Kind() == reflect.Map {
		panic("Values must be map[string]interface{}")
	}

	object := reflect.ValueOf(values)
	if object.Kind() == reflect.Ptr {
		object = object.Elem()
	}
	return c.generateParamString(object.NumField())

}

func (c *PostgresPersistence) generateParamString(paramNum int) string {
	result := strings.Builder{}
	for index := 1; index <= paramNum; index++ {
		if result.String() != "" {
			result.WriteString(",")
		}
		result.WriteString("$")
		result.WriteString(strconv.FormatInt((int64)(index), 16))
	}

	return result.String()
}

// Generates a list of column sets to use in UPDATE statements like: column1=$1,column2=$2
// - values a key-value map with columns and values
// Returns a generated list of column sets
func (c *PostgresPersistence) GenerateSetParameters(values interface{}) string {

	result := strings.Builder{}
	// String arrays
	if val, ok := values.([]string); ok {
		for index, column := range val {
			if result.String() != "" {
				result.WriteString(",")
			}
			result.WriteString(c.QuoteIdentifier(column) + "=$" + strconv.FormatInt((int64)(index+1), 16))
		}
		return result.String()
	}

	if reflect.TypeOf(values).Kind() == reflect.Array {
		panic("Values must be string array")
	}

	if val, ok := values.(map[string]interface{}); ok {
		index := 1
		for column, _ := range val {
			if result.String() != "" {
				result.WriteString(",")
			}
			result.WriteString(c.QuoteIdentifier(column) + "=$" + strconv.FormatInt((int64)(index), 16))
			index++
		}
		return result.String()
	}

	if reflect.TypeOf(values).Kind() == reflect.Map {
		panic("Values must be map[string]interface{}")
	}

	object := reflect.ValueOf(values)
	if object.Kind() == reflect.Ptr {
		object = object.Elem()
	}
	typ := object.Type()
	for i := 0; i < object.NumField(); i++ {
		if result.String() != "" {
			result.WriteString(",")
		}
		result.WriteString(c.QuoteIdentifier(typ.Field(i).Name) + "=$" + strconv.FormatInt((int64)(i+1), 16))
	}
	return result.String()

}

// Generates a list of column parameters
// - values a key-value map with columns and values
// Returns a generated list of column values
func (c *PostgresPersistence) GenerateValues(values interface{}) []interface{} {
	results := make([]interface{}, 0, 1)

	if val, ok := values.(map[string]interface{}); ok {
		for _, item := range val {
			results = append(results, item)
		}
		return results
	}

	if reflect.TypeOf(values).Kind() == reflect.Map {
		panic("Values must be map[string]interface{}")
	}

	object := reflect.ValueOf(values)
	if object.Kind() == reflect.Ptr {
		object = object.Elem()
	}
	for i := 0; i < object.NumField(); i++ {
		results = append(results, object.Field(i).Interface())
	}

	return results
}

// Gets a page of data items retrieved by a given filter and sorted according to sort parameters.
// This method shall be called by a func (c * PostgresPersistence) getPageByFilter method from child class that
// receives FilterParams and converts them into a filter function.
// - correlationId     (optional) transaction id to trace execution through call chain.
// - filter            (optional) a filter JSON object
// - paging            (optional) paging parameters
// - sort              (optional) sorting JSON object
// - select            (optional) projection JSON object
// - Returns           receives a data page or error.
func (c *PostgresPersistence) GetPageByFilter(correlationId string, filter interface{}, paging *cdata.PagingParams,
	sort interface{}, sel interface{}) (page *cdata.DataPage, err error) {

	query := "SELECT * FROM " + c.QuoteIdentifier(c.TableName)
	if sel != nil {
		if slct, ok := sel.(string); ok && slct != "" {
			query = "SELECT " + slct + " FROM " + c.QuoteIdentifier(c.TableName)
		}
	}

	// Adjust max item count based on configurationpaging
	if paging == nil {
		paging = cdata.NewEmptyPagingParams()
	}
	skip := paging.GetSkip(-1)
	take := paging.GetTake((int64)(c.MaxPageSize))
	pagingEnabled := paging.Total

	if filter != nil {
		if flt, ok := filter.(string); ok && flt != "" {
			query += " WHERE " + flt
		}
	}

	if sort != nil {
		if srt, ok := sort.(string); ok && srt != "" {
			query += " ORDER BY " + srt
		}
	}

	if skip >= 0 {
		query += " OFFSET " + strconv.FormatInt(skip, 10)
	}

	query += " LIMIT " + strconv.FormatInt(take, 10)
	qResult, qErr := c.Client.Query(context.TODO(), query)

	if qErr != nil {
		return nil, qErr
	}

	items := make([]interface{}, 0, 0)
	for qResult.Next() {
		rows, vErr := qResult.Values()
		if vErr != nil {
			continue
		}
		item := c.ConvertFromRows(qResult.FieldDescriptions(), rows)
		items = append(items, item)
	}

	if items != nil {
		c.Logger.Trace(correlationId, "Retrieved %d from %s", len(items), c.TableName)
	}

	if pagingEnabled {
		query := "SELECT COUNT(*) AS count FROM " + c.QuoteIdentifier(c.TableName)
		if filter != nil {
			if flt, ok := sel.(string); ok && flt != "" {
				query += " WHERE " + flt
			}
		}
		result, qErr := c.Client.Query(context.TODO(), query)
		if qErr != nil {
			return nil, qErr
		}
		var count int64 = 0
		if result != nil && result.Next() {
			rows, _ := result.Values()
			if len(rows) == 1 {
				count = cconv.LongConverter.ToLong(rows[0])
			}
		}
		page = cdata.NewDataPage(&count, items)
		return page, nil
	}
	var total int64 = 0
	page = cdata.NewDataPage(&total, items)
	return page, nil
}

// Gets a number of data items retrieved by a given filter.
// This method shall be called by a func (c * PostgresPersistence) getCountByFilter method from child class that
// receives FilterParams and converts them into a filter function.
// - correlationId     (optional) transaction id to trace execution through call chain.
// - filter            (optional) a filter JSON object
// - Returns           data page or error.
func (c *PostgresPersistence) GetCountByFilter(correlationId string, filter interface{}) (count int64, err error) {

	query := "SELECT COUNT(*) AS count FROM " + c.QuoteIdentifier(c.TableName)

	if filter != nil {
		if flt, ok := filter.(string); ok && flt != "" {
			query += " WHERE " + flt
		}
	}

	result, qErr := c.Client.Query(context.TODO(), query)
	if qErr != nil {
		return 0, qErr
	}

	count = 0
	if result != nil && result.Next() {
		rows, _ := result.Values()
		if len(rows) == 1 {
			count = cconv.LongConverter.ToLong(rows[0])
		}
	}
	if count != 0 {
		c.Logger.Trace(correlationId, "Counted %d items in %s", count, c.TableName)
	}

	return count, nil
}

// Gets a list of data items retrieved by a given filter and sorted according to sort parameters.
// This method shall be called by a func (c * PostgresPersistence) getListByFilter method from child class that
// receives FilterParams and converts them into a filter function.
// - correlationId    (optional) transaction id to trace execution through call chain.
// - filter           (optional) a filter JSON object
// - paging           (optional) paging parameters
// - sort             (optional) sorting JSON object
// - select           (optional) projection JSON object
// - Returns          data list or error.
func (c *PostgresPersistence) GetListByFilter(correlationId string, filter interface{}, sort interface{}, sel interface{}) (items []interface{}, err error) {

	query := "SELECT * FROM " + c.QuoteIdentifier(c.TableName)
	if sel != nil {
		if slct, ok := sel.(string); ok && slct != "" {
			query = "SELECT " + slct + " FROM " + c.QuoteIdentifier(c.TableName)
		}
	}

	if filter != nil {
		if flt, ok := filter.(string); ok && flt != "" {
			query += " WHERE " + flt
		}
	}

	if sort != nil {
		if srt, ok := sort.(string); ok && srt != "" {
			query += " ORDER BY " + srt
		}
	}

	result, qErr := c.Client.Query(context.TODO(), query)

	if qErr != nil {
		return nil, qErr
	}

	items = make([]interface{}, 0, 1)
	for result.Next() {
		rows, vErr := result.Values()
		if vErr != nil {
			continue
		}
		item := c.ConvertFromRows(result.FieldDescriptions(), rows)
		items = append(items, item)
	}

	if items != nil {
		c.Logger.Trace(correlationId, "Retrieved %d from %s", len(items), c.TableName)
	}
	return items, nil
}

// Gets a random item from items that match to a given filter.
// This method shall be called by a func (c * PostgresPersistence) getOneRandom method from child class that
// receives FilterParams and converts them into a filter function.
// - correlationId     (optional) transaction id to trace execution through call chain.
// - filter            (optional) a filter JSON object
// - Returns            random item or error.
func (c *PostgresPersistence) GetOneRandom(correlationId string, filter interface{}) (item interface{}, err error) {

	query := "SELECT COUNT(*) AS count FROM " + c.QuoteIdentifier(c.TableName)

	if filter != nil {
		if flt, ok := filter.(string); ok && flt != "" {
			query += " WHERE " + flt
		}
	}

	result, qErr := c.Client.Query(context.TODO(), query)
	if qErr != nil {
		return nil, qErr
	}

	query = "SELECT * FROM " + c.QuoteIdentifier(c.TableName)

	if filter != nil {
		if flt, ok := filter.(string); ok && flt != "" {
			query += " WHERE " + flt
		}
	}

	var count int64 = 0
	if result != nil && result.Next() {
		rows, _ := result.Values()
		if len(rows) == 1 {
			if row, ok := rows[0].(map[string]interface{}); ok {
				count = cconv.LongConverter.ToLong(row["count"])
			}
		}
	}

	rand.Seed(time.Now().UnixNano())
	pos := rand.Int63n(int64(count))
	query += " OFFSET " + strconv.FormatInt(pos, 10) + " LIMIT 1"
	result, qErr = c.Client.Query(context.TODO(), query)
	if qErr != nil {
		return nil, qErr
	}
	if result.Next() {
		rows, vErr := result.Values()
		if vErr == nil {
			item := c.ConvertFromRows(result.FieldDescriptions(), rows)
			c.Logger.Trace(correlationId, "Retrieved random item from %s", c.TableName)
			return item, nil
		}
		return nil, vErr
	}
	c.Logger.Trace(correlationId, "Random item wasn't found from %s", c.TableName)
	return nil, nil
}

// Creates a data item.
// - correlation_id    (optional) transaction id to trace execution through call chain.
// - item              an item to be created.
// - Returns          (optional) callback function that receives created item or error.
func (c *PostgresPersistence) Create(correlationId string, item interface{}) (result interface{}, err error) {

	if item == nil {
		return nil, nil
	}

	row := c.ConvertFromPublic(item)
	columns := c.GenerateColumns(row)
	params := c.GenerateParameters(row)
	values := c.GenerateValues(row)
	query := "INSERT INTO " + c.QuoteIdentifier(c.TableName) + " (" + columns + ") VALUES (" + params + ") RETURNING *"
	results, qErr := c.Client.Query(context.TODO(), query, values...)

	if qErr == nil && results.Next() {
		rows, vErr := results.Values()
		if vErr != nil {
			return nil, vErr
		}

		item := c.ConvertFromRows(results.FieldDescriptions(), rows)
		id := cmpersist.GetObjectId(item)
		c.Logger.Trace(correlationId, "Created in %s with id = %s", c.TableName, id)
		return item, nil
	}
	return nil, qErr
}

// Deletes data items that match to a given filter.
// This method shall be called by a func (c * PostgresPersistence) deleteByFilter method from child class that
// receives FilterParams and converts them into a filter function.
// - correlationId     (optional) transaction id to trace execution through call chain.
// - filter            (optional) a filter JSON object.
// - Returns           error or nil for success.
func (c *PostgresPersistence) DeleteByFilter(correlationId string, filter string) (err error) {
	query := "DELETE FROM " + c.QuoteIdentifier(c.TableName)
	if filter != "" {
		query += " WHERE " + filter
	}

	result, qErr := c.Client.Query(context.TODO(), query)

	if qErr == nil {
		var count int64 = 0
		if result != nil && result.Next() {
			rows, _ := result.Values()
			if len(rows) == 1 {
				count = cconv.LongConverter.ToLong(rows[0])
			}
		}
		c.Logger.Trace(correlationId, "Deleted %d items from %s", count, c.TableName)
	}
	return qErr
}

// service function for return pointer on new prototype object for unmarshaling
func (c *PostgresPersistence) NewObjectByPrototype() reflect.Value {
	proto := c.Prototype
	if proto.Kind() == reflect.Ptr {
		proto = proto.Elem()
	}
	return reflect.New(proto)
}

func (c *PostgresPersistence) ConvertResultToPublic(docPointer reflect.Value) interface{} {
	item := docPointer.Elem().Interface()
	// TODO: decide how call nedded instance of ConvertToPublic
	//c.ConvertToPublic(&item)
	if c.Prototype.Kind() == reflect.Ptr {
		return docPointer.Interface()
	}
	return item
}

// ConvertFromMap method are converts from map[string]interface{} to object, defined by c.Prototype
func (c *PostgresPersistence) ConvertFromRows(columns []pgproto3.FieldDescription, rows []interface{}) interface{} {

	buf := make(map[string]interface{}, 0)

	for index, column := range columns {
		buf[(string)(column.Name)] = rows[index]
	}

	docPointer := c.NewObjectByPrototype()
	jsonBuf, _ := json.Marshal(buf)
	json.Unmarshal(jsonBuf, docPointer.Interface())
	return c.ConvertResultToPublic(docPointer)
}