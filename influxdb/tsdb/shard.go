package tsdb

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/influxdata/influxdb/influxql"
	"github.com/influxdata/influxdb/models"
	internal "github.com/influxdata/influxdb/tsdb/internal"
)

// monitorStatInterval is the interval at which the shard is inspected
// for the purpose of determining certain monitoring statistics.
const monitorStatInterval = 30 * time.Second

const (
	statWriteReq        = "writeReq"
	statSeriesCreate    = "seriesCreate"
	statFieldsCreate    = "fieldsCreate"
	statWritePointsFail = "writePointsFail"
	statWritePointsOK   = "writePointsOk"
	statWriteBytes      = "writeBytes"
	statDiskBytes       = "diskBytes"
)

var (
	// ErrFieldOverflow is returned when too many fields are created on a measurement.
	ErrFieldOverflow = errors.New("field overflow")

	// ErrFieldTypeConflict is returned when a new field already exists with a different type.
	ErrFieldTypeConflict = errors.New("field type conflict")

	// ErrFieldNotFound is returned when a field cannot be found.
	ErrFieldNotFound = errors.New("field not found")

	// ErrFieldUnmappedID is returned when the system is presented, during decode, with a field ID
	// there is no mapping for.
	ErrFieldUnmappedID = errors.New("field ID not mapped")

	// ErrEngineClosed is returned when a caller attempts indirectly to
	// access the shard's underlying engine.
	ErrEngineClosed = errors.New("engine is closed")

	// ErrShardDisabled is returned when a the shard is not available for
	// queries or writes.
	ErrShardDisabled = errors.New("shard is disabled")
)

// A ShardError implements the error interface, and contains extra
// context about the shard that generated the error.
type ShardError struct {
	id  uint64
	Err error
}

// NewShardError returns a new ShardError.
func NewShardError(id uint64, err error) error {
	if err == nil {
		return nil
	}
	return ShardError{id: id, Err: err}
}

func (e ShardError) Error() string {
	return fmt.Sprintf("[shard %d] %s", e.id, e.Err)
}

// Shard represents a self-contained time series database. An inverted index of
// the measurement and tag data is kept along with the raw time series data.
// Data can be split across many shards. The query engine in TSDB is responsible
// for combining the output of many shards into a single query result.

// shard 对应磁盘上的一批 tsm 文件，其中存储的是一些 series 在一个指定的时间范围内的所有数据
type Shard struct {
	index   *DatabaseIndex		// 所在数据库的索引对象
	path    string				// shard 在磁盘上的路径
	walPath string				// 对应的 wal 文件所在目录
	id      uint64				// shard ID，就是在磁盘上的文件名

	database        string		// 所在数据库名
	retentionPolicy string		// 对应存储策略名

	options EngineOptions		// 存储引擎选项

	mu      sync.RWMutex
	engine  Engine				// 存储引擎
	closing chan struct{}
	enabled bool

	// expvar-based stats.
	stats    *ShardStatistics
	statTags models.Tags			// 统计信息的 tags

	logger *log.Logger

	// The writer used by the logger.
	LogOutput    io.Writer
	EnableOnOpen bool				// 是否创建成功之后就可读可写，默认为 true
}

// NewShard returns a new initialized Shard. walPath doesn't apply to the b1 type index
// 根据指定的 id 创建一个新的 shard 对象
func NewShard(id uint64, index *DatabaseIndex, path string, walPath string, options EngineOptions) *Shard {
	db, rp := DecodeStorePath(path)
	s := &Shard{
		index:   index,
		id:      id,
		path:    path,
		walPath: walPath,
		options: options,
		closing: make(chan struct{}),

		stats: &ShardStatistics{},
		statTags: map[string]string{
			"path":            path,
			"id":              fmt.Sprintf("%d", id),
			"database":        db,
			"retentionPolicy": rp,
		},

		database:        db,
		retentionPolicy: rp,

		LogOutput:    os.Stderr,
		EnableOnOpen: true,
	}

	s.SetLogOutput(os.Stderr)
	return s
}

// SetLogOutput sets the writer to which log output will be written. It must
// not be called after the Open method has been called.
func (s *Shard) SetLogOutput(w io.Writer) {
	s.LogOutput = w
	s.logger = log.New(w, "[shard] ", log.LstdFlags)
	if err := s.ready(); err == nil {
		s.engine.SetLogOutput(w)
	}
}

// SetEnabled enables the shard for queries and write.  When disabled, all
// writes and queries return an error and compactions are stopped for the shard.
func (s *Shard) SetEnabled(enabled bool) {
	s.mu.Lock()
	// Prevent writes and queries
	s.enabled = enabled
	if s.engine != nil {
		// Disable background compactions and snapshotting
		s.engine.SetEnabled(enabled)
	}
	s.mu.Unlock()
}

// ShardStatistics maintains statistics for a shard.
// shard 的统计信息
type ShardStatistics struct {
	WriteReq        int64
	SeriesCreated   int64
	FieldsCreated   int64
	WritePointsFail int64
	WritePointsOK   int64
	BytesWritten    int64
	DiskBytes       int64
}

// Statistics returns statistics for periodic monitoring.
func (s *Shard) Statistics(tags map[string]string) []models.Statistic {
	if err := s.ready(); err != nil {
		return nil
	}

	tags = s.statTags.Merge(tags)
	statistics := []models.Statistic{{
		Name: "shard",
		Tags: models.Tags(tags).Merge(map[string]string{"engine": s.options.EngineVersion}),
		Values: map[string]interface{}{
			statWriteReq:        atomic.LoadInt64(&s.stats.WriteReq),
			statSeriesCreate:    atomic.LoadInt64(&s.stats.SeriesCreated),
			statFieldsCreate:    atomic.LoadInt64(&s.stats.FieldsCreated),
			statWritePointsFail: atomic.LoadInt64(&s.stats.WritePointsFail),
			statWritePointsOK:   atomic.LoadInt64(&s.stats.WritePointsOK),
			statWriteBytes:      atomic.LoadInt64(&s.stats.BytesWritten),
			statDiskBytes:       atomic.LoadInt64(&s.stats.DiskBytes),
		},
	}}
	statistics = append(statistics, s.engine.Statistics(tags)...)
	return statistics
}

// Path returns the path set on the shard when it was created.
func (s *Shard) Path() string { return s.path }

// Open initializes and opens the shard's store.
// 创建 shard 的底层存储引擎对象，初始化 wal, tsm file, cache 等管理对象的服务，从 tsm file 中获取信息建立 measurement 以及 tags, filed 相关的在内存中的索引信息
func (s *Shard) Open() error {
	if err := func() error {
		s.mu.Lock()
		defer s.mu.Unlock()

		// Return if the shard is already open
		// 如果 shard 的存储引擎已经创建，直接返回
		if s.engine != nil {
			return nil
		}

		// Initialize underlying engine.
		// 创建此 shard 的存储引擎
		e, err := NewEngine(s.path, s.walPath, s.options)
		if err != nil {
			return err
		}

		// Set log output on the engine.
		e.SetLogOutput(s.LogOutput)

		// Disable compactions while loading the index
		// 在创建索引过程中禁止压缩
		e.SetEnabled(false)

		// Open engine.
		// 初始化存储相关的服务，包括 WAL, TSM file, Cache 等对象管理服务
		if err := e.Open(); err != nil {
			return err
		}

		// Load metadata index.
		start := time.Now()
		// 指定的 shard 中的每一个文件的索引信息中加载所有 key 的Name，之后解析出 measurement 和 tags 并将其在内存中按照特定的数据结构做一个缓存
		if err := e.LoadMetadataIndex(s.id, s.index); err != nil {
			return err
		}

		// 获取此 shard 中所有 series 的数量
		count := s.index.SeriesShardN(s.id)
		atomic.AddInt64(&s.stats.SeriesCreated, int64(count))

		s.engine = e

		s.logger.Printf("%s database index loaded in %s", s.path, time.Now().Sub(start))

		go s.monitorSize()

		return nil
	}(); err != nil {
		s.close()
		return NewShardError(s.id, err)
	}

	// 索引信息建立完成，可以启用
	if s.EnableOnOpen {
		// enable writes, queries and compactions
		s.SetEnabled(true)
	}

	return nil
}

// Close shuts down the shard's store.
func (s *Shard) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.close()
}

func (s *Shard) close() error {
	if s.engine == nil {
		return nil
	}

	// Close the closing channel at most once.
	select {
	case <-s.closing:
	default:
		close(s.closing)
	}

	// Don't leak our shard ID and series keys in the index
	s.index.RemoveShard(s.id)

	err := s.engine.Close()
	if err == nil {
		s.engine = nil
	}
	return err
}

// ready determines if the Shard is ready for queries or writes.
// It returns nil if ready, otherwise ErrShardClosed or ErrShardDiabled
// 返回该 shard 是否可读写
func (s *Shard) ready() error {
	var err error

	s.mu.RLock()
	if s.engine == nil {
		err = ErrEngineClosed
	} else if !s.enabled {
		err = ErrShardDisabled
	}
	s.mu.RUnlock()
	return err
}

// DiskSize returns the size on disk of this shard
// 获取在磁盘上实际占用的空间
func (s *Shard) DiskSize() (int64, error) {
	var size int64
	err := filepath.Walk(s.path, func(_ string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !fi.IsDir() {
			size += fi.Size()
		}
		return err
	})
	if err != nil {
		return 0, err
	}

	err = filepath.Walk(s.walPath, func(_ string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !fi.IsDir() {
			size += fi.Size()
		}
		return err
	})

	return size, err
}

// FieldCreate holds information for a field to create on a measurement
type FieldCreate struct {
	Measurement string
	Field       *Field
}

// SeriesCreate holds information for a series to create
type SeriesCreate struct {
	Measurement string
	Series      *Series
}

// WritePoints will write the raw data points and any new metadata to the index in the shard
// 将 Points 信息写入此 shard
func (s *Shard) WritePoints(points []models.Point) error {
	if err := s.ready(); err != nil {
		return err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	atomic.AddInt64(&s.stats.WriteReq, 1)

	// 检查要写入的 Points 中是否有新的 series 和 field，如果有，需要更新元数据信息以及索引信息，并且返回需要创建的 field 信息
	fieldsToCreate, err := s.validateSeriesAndFields(points)
	if err != nil {
		return err
	}
	atomic.AddInt64(&s.stats.FieldsCreated, int64(len(fieldsToCreate)))

	// add any new fields and keep track of what needs to be saved
	// 在内存索引中加入 fields 信息
	if err := s.createFieldsAndMeasurements(fieldsToCreate); err != nil {
		return err
	}

	// Write to the engine.
	// 调用此 shard 的存储引擎的写入函数，tsm1 中先写入 memtable，之后写入 wal 文件中
	if err := s.engine.WritePoints(points); err != nil {
		atomic.AddInt64(&s.stats.WritePointsFail, 1)
		return fmt.Errorf("engine: %s", err)
	}
	atomic.AddInt64(&s.stats.WritePointsOK, int64(len(points)))

	return nil
}

// 检查此 shard 中是否含有指定的 seriesKey
func (s *Shard) ContainsSeries(seriesKeys []string) (map[string]bool, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}

	return s.engine.ContainsSeries(seriesKeys)
}

// DeleteSeries deletes a list of series.
func (s *Shard) DeleteSeries(seriesKeys []string) error {
	if err := s.ready(); err != nil {
		return err
	}
	if err := s.engine.DeleteSeries(seriesKeys); err != nil {
		return err
	}
	return nil
}

// DeleteSeriesRange deletes all values from for seriesKeys between min and max (inclusive)
func (s *Shard) DeleteSeriesRange(seriesKeys []string, min, max int64) error {
	if err := s.ready(); err != nil {
		return err
	}

	if err := s.engine.DeleteSeriesRange(seriesKeys, min, max); err != nil {
		return err
	}

	return nil
}

// DeleteMeasurement deletes a measurement and all underlying series.
func (s *Shard) DeleteMeasurement(name string, seriesKeys []string) error {
	if err := s.ready(); err != nil {
		return err
	}

	if err := s.engine.DeleteMeasurement(name, seriesKeys); err != nil {
		return err
	}

	return nil
}

// 在内存索引中加入 fields 信息
func (s *Shard) createFieldsAndMeasurements(fieldsToCreate []*FieldCreate) error {
	if len(fieldsToCreate) == 0 {
		return nil
	}

	// add fields
	for _, f := range fieldsToCreate {
		m := s.engine.MeasurementFields(f.Measurement)

		// Add the field to the in memory index
		if err := m.CreateFieldIfNotExists(f.Field.Name, f.Field.Type, false); err != nil {
			return err
		}

		// ensure the measurement is in the index and the field is there
		// 在数据库索引中加上 measurement 以及 filed 的信息
		measurement := s.index.CreateMeasurementIndexIfNotExists(f.Measurement)
		measurement.SetFieldName(f.Field.Name)
	}

	return nil
}

// validateSeriesAndFields checks which series and fields are new and whose metadata should be saved and indexed
// 检查要写入的 Points 中是否有新的 series 和 field，如果有，需要更新元数据信息以及索引信息，并且返回需要创建的 field 信息
func (s *Shard) validateSeriesAndFields(points []models.Point) ([]*FieldCreate, error) {
	var fieldsToCreate []*FieldCreate

	// get the shard mutex for locally defined fields
	for _, p := range points {
		// see if the series should be added to the index
		// 获取该 Point 的 seriesKey
		key := string(p.Key())
		// 检查当前索引中是否存在，不存在就创建一个
		ss := s.index.Series(key)
		if ss == nil {
			ss = NewSeries(key, p.Tags())
			atomic.AddInt64(&s.stats.SeriesCreated, 1)
		}

		// 不存在就创建一个，索引信息中记录下在当前这个 shard 中存在此 series
		ss = s.index.CreateSeriesIndexIfNotExists(p.Name(), ss)
		s.index.AssignShard(ss.Key, s.id)

		// see if the field definitions need to be saved to the shard
		mf := s.engine.MeasurementFields(p.Name())
		// 上面这个函数并不会返回 nil，因为不存在的话会创建一个空的对象，所以下面的判断没有必要
		// 不过由于存储引擎可替换，可能后续使用其他引擎时可能实现机制不一样
		if mf == nil {
			for name, value := range p.Fields() {
				fieldsToCreate = append(fieldsToCreate, &FieldCreate{p.Name(), &Field{Name: name, Type: influxql.InspectDataType(value)}})
			}
			continue // skip validation since all fields are new
		}

		// validate field types and encode data
		// 检查已经存在的 field 类型是否不一致
		for name, value := range p.Fields() {
			if f := mf.Field(name); f != nil {
				// Field present in shard metadata, make sure there is no type conflict.
				if f.Type != influxql.InspectDataType(value) {
					return nil, fmt.Errorf("field type conflict: input field \"%s\" on measurement \"%s\" is type %T, already exists as type %s", name, p.Name(), value, f.Type)
				}

				continue // Field is present, and it's of the same type. Nothing more to do.
			}

			fieldsToCreate = append(fieldsToCreate, &FieldCreate{p.Name(), &Field{Name: name, Type: influxql.InspectDataType(value)}})
		}
	}

	return fieldsToCreate, nil
}

// SeriesCount returns the number of series buckets on the shard.
func (s *Shard) SeriesCount() (int, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	return s.engine.SeriesCount()
}

// WriteTo writes the shard's data to w.
// 将 shard 中的数据写入 w
func (s *Shard) WriteTo(w io.Writer) (int64, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	n, err := s.engine.WriteTo(w)
	atomic.AddInt64(&s.stats.BytesWritten, int64(n))
	return n, err
}

// CreateIterator returns an iterator for the data in the shard.
// 创建 shard 数据的迭代器
func (s *Shard) CreateIterator(opt influxql.IteratorOptions) (influxql.Iterator, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}

	if influxql.Sources(opt.Sources).HasSystemSource() {
		return s.createSystemIterator(opt)
	}
	opt.Sources = influxql.Sources(opt.Sources).Filter(s.database, s.retentionPolicy)
	return s.engine.CreateIterator(opt)
}

// createSystemIterator returns an iterator for a system source.
// 根据查询数据源判断，如果是系统资源的查询，返回系统数据迭代器，这些数据通常从内存中直接获取
func (s *Shard) createSystemIterator(opt influxql.IteratorOptions) (influxql.Iterator, error) {
	// Only support a single system source.
	if len(opt.Sources) > 1 {
		return nil, errors.New("cannot select from multiple system sources")
	}

	m := opt.Sources[0].(*influxql.Measurement)
	switch m.Name {
	case "_fieldKeys":
		return NewFieldKeysIterator(s, opt)
	case "_measurements":
		return NewMeasurementIterator(s.index, opt)
	case "_series":
		return NewSeriesIterator(s, opt)
	case "_tagKeys":
		return NewTagKeysIterator(s, opt)
	case "_tags":
		return NewTagValuesIterator(s, opt)
	default:
		return nil, fmt.Errorf("unknown system source: %s", m.Name)
	}
}

// FieldDimensions returns unique sets of fields and dimensions across a list of sources.
// 根据数据源，field 的类型，如果是非系统资源，返回可以用于切分的 tagk
func (s *Shard) FieldDimensions(sources influxql.Sources) (fields map[string]influxql.DataType, dimensions map[string]struct{}, err error) {
	if err := s.ready(); err != nil {
		return nil, nil, err
	}

	if influxql.Sources(sources).HasSystemSource() {
		// Only support a single system source.
		if len(sources) > 1 {
			return nil, nil, errors.New("cannot select from multiple system sources")
		}

		switch m := sources[0].(type) {
		case *influxql.Measurement:
			switch m.Name {
			case "_fieldKeys":
				return map[string]influxql.DataType{
					"fieldKey":  influxql.String,
					"fieldType": influxql.String,
				}, nil, nil
			case "_measurements":
				return map[string]influxql.DataType{"_name": influxql.String}, nil, nil
			case "_series":
				return map[string]influxql.DataType{"key": influxql.String}, nil, nil
			case "_tagKeys":
				return map[string]influxql.DataType{"tagKey": influxql.String}, nil, nil
			case "_tags":
				return map[string]influxql.DataType{
					"_tagKey": influxql.String,
					"value":   influxql.String,
				}, nil, nil
			}
		}
		return nil, nil, nil
	}

	fields = make(map[string]influxql.DataType)
	dimensions = make(map[string]struct{})

	for _, src := range sources {
		switch m := src.(type) {
		case *influxql.Measurement:
			// Retrieve measurement.
			mm := s.index.Measurement(m.Name)
			if mm == nil {
				continue
			}

			// Append fields and dimensions.
			mf := s.engine.MeasurementFields(m.Name)
			if mf != nil {
				for name, typ := range mf.FieldSet() {
					fields[name] = typ
				}
			}
			for _, key := range mm.TagKeys() {
				dimensions[key] = struct{}{}
			}
		}
	}

	return
}

// ExpandSources expands regex sources and removes duplicates.
// NOTE: sources must be normalized (db and rp set) before calling this function.
// 如果数据源是一个匹配表达式，过滤出实际的数据源
func (s *Shard) ExpandSources(sources influxql.Sources) (influxql.Sources, error) {
	// Use a map as a set to prevent duplicates.
	set := map[string]influxql.Source{}

	// Iterate all sources, expanding regexes when they're found.
	for _, source := range sources {
		switch src := source.(type) {
		case *influxql.Measurement:
			// Add non-regex measurements directly to the set.
			if src.Regex == nil {
				set[src.String()] = src
				continue
			}

			// Loop over matching measurements.
			for _, m := range s.index.MeasurementsByRegex(src.Regex.Val) {
				other := &influxql.Measurement{
					Database:        src.Database,
					RetentionPolicy: src.RetentionPolicy,
					Name:            m.Name,
				}
				set[other.String()] = other
			}

		default:
			return nil, fmt.Errorf("expandSources: unsupported source type: %T", source)
		}
	}

	// Convert set to sorted slice.
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)

	// Convert set to a list of Sources.
	expanded := make(influxql.Sources, 0, len(set))
	for _, name := range names {
		expanded = append(expanded, set[name])
	}

	return expanded, nil
}

// Restore restores data to the underlying engine for the shard.
// The shard is reopened after restore.
func (s *Shard) Restore(r io.Reader, basePath string) error {
	s.mu.Lock()

	// Restore to engine.
	if err := s.engine.Restore(r, basePath); err != nil {
		s.mu.Unlock()
		return err
	}

	s.mu.Unlock()

	// Close shard.
	if err := s.Close(); err != nil {
		return err
	}

	// Reopen engine.
	return s.Open()
}

// CreateSnapshot will return a path to a temp directory
// containing hard links to the underlying shard files
// 为此 shard 中的所有文件创建硬链接，返回一个临时目录，用于存放这些链接
func (s *Shard) CreateSnapshot() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.engine.CreateSnapshot()
}

// 定期获取磁盘大小
func (s *Shard) monitorSize() {
	// 定期获取磁盘大小
	t := time.NewTicker(monitorStatInterval)
	defer t.Stop()
	for {
		select {
		case <-s.closing:
			return
		case <-t.C:
			size, err := s.DiskSize()
			if err != nil {
				s.logger.Printf("Error collecting shard size: %v", err)
				continue
			}
			atomic.StoreInt64(&s.stats.DiskBytes, size)
		}
	}
}

// Shards represents a sortable list of shards.
type Shards []*Shard

func (a Shards) Len() int           { return len(a) }
func (a Shards) Less(i, j int) bool { return a[i].id < a[j].id }
func (a Shards) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

// MeasurementFields holds the fields of a measurement and their codec.
// 一个 measurement 中存在的所有的 field 对象
type MeasurementFields struct {
	mu sync.RWMutex

	fields map[string]*Field
}

func NewMeasurementFields() *MeasurementFields {
	return &MeasurementFields{fields: make(map[string]*Field)}
}

// MarshalBinary encodes the object to a binary format.
func (m *MeasurementFields) MarshalBinary() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var pb internal.MeasurementFields
	for _, f := range m.fields {
		id := int32(f.ID)
		name := f.Name
		t := int32(f.Type)
		pb.Fields = append(pb.Fields, &internal.Field{ID: &id, Name: &name, Type: &t})
	}
	return proto.Marshal(&pb)
}

// UnmarshalBinary decodes the object from a binary format.
func (m *MeasurementFields) UnmarshalBinary(buf []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var pb internal.MeasurementFields
	if err := proto.Unmarshal(buf, &pb); err != nil {
		return err
	}
	m.fields = make(map[string]*Field, len(pb.Fields))
	for _, f := range pb.Fields {
		m.fields[f.GetName()] = &Field{ID: uint8(f.GetID()), Name: f.GetName(), Type: influxql.DataType(f.GetType())}
	}
	return nil
}

// CreateFieldIfNotExists creates a new field with an autoincrementing ID.
// Returns an error if 255 fields have already been created on the measurement or
// the fields already exists with a different type.
// 创建一个新的 field 对象，并且获取一个递增的 ID 值，同一个 measurement 中的 field 值不能超过 255 个，并且同一个 fieldName 的类型唯一
func (m *MeasurementFields) CreateFieldIfNotExists(name string, typ influxql.DataType, limitCount bool) error {
	m.mu.RLock()

	// Ignore if the field already exists.
	if f := m.fields[name]; f != nil {
		if f.Type != typ {
			m.mu.RUnlock()
			return ErrFieldTypeConflict
		}
		m.mu.RUnlock()
		return nil
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if f := m.fields[name]; f != nil {
		return nil
	}

	// Create and append a new field.
	f := &Field{
		ID:   uint8(len(m.fields) + 1),
		Name: name,
		Type: typ,
	}
	m.fields[name] = f

	return nil
}

func (m *MeasurementFields) Field(name string) *Field {
	m.mu.RLock()
	f := m.fields[name]
	m.mu.RUnlock()
	return f
}

func (m *MeasurementFields) FieldSet() map[string]influxql.DataType {
	m.mu.RLock()
	defer m.mu.RUnlock()

	fields := make(map[string]influxql.DataType)
	for name, f := range m.fields {
		fields[name] = f.Type
	}
	return fields
}

// Field represents a series field.
// field 对象，包括 id, fieldName, 以及类型
type Field struct {
	ID   uint8             `json:"id,omitempty"`
	Name string            `json:"name,omitempty"`
	Type influxql.DataType `json:"type,omitempty"`
}

// shardIteratorCreator creates iterators for a local shard.
// This simply wraps the shard so that Close() does not close the underlying shard.
type shardIteratorCreator struct {
	sh *Shard
}

func (ic *shardIteratorCreator) Close() error { return nil }

func (ic *shardIteratorCreator) CreateIterator(opt influxql.IteratorOptions) (influxql.Iterator, error) {
	return ic.sh.CreateIterator(opt)
}
func (ic *shardIteratorCreator) FieldDimensions(sources influxql.Sources) (fields map[string]influxql.DataType, dimensions map[string]struct{}, err error) {
	return ic.sh.FieldDimensions(sources)
}
func (ic *shardIteratorCreator) ExpandSources(sources influxql.Sources) (influxql.Sources, error) {
	return ic.sh.ExpandSources(sources)
}

func NewFieldKeysIterator(sh *Shard, opt influxql.IteratorOptions) (influxql.Iterator, error) {
	itr := &fieldKeysIterator{sh: sh}

	// Retrieve measurements from shard. Filter if condition specified.
	if opt.Condition == nil {
		itr.mms = sh.index.Measurements()
	} else {
		mms, _, err := sh.index.measurementsByExpr(opt.Condition)
		if err != nil {
			return nil, err
		}
		itr.mms = mms
	}

	// Sort measurements by name.
	sort.Sort(itr.mms)

	return itr, nil
}

// fieldKeysIterator iterates over measurements and gets field keys from each measurement.
// 从每个 measurement 获取 filed key 的迭代器
type fieldKeysIterator struct {
	sh  *Shard
	mms Measurements // remaining measurements
	// 用于缓存当前遍历的部分数据
	buf struct {
		mm     *Measurement // current measurement
		fields []Field      // current measurement's fields
	}
}

// Stats returns stats about the points processed.
func (itr *fieldKeysIterator) Stats() influxql.IteratorStats { return influxql.IteratorStats{} }

// Close closes the iterator.
func (itr *fieldKeysIterator) Close() error { return nil }

// Next emits the next tag key name.
func (itr *fieldKeysIterator) Next() (*influxql.FloatPoint, error) {
	for {
		// If there are no more keys then move to the next measurements.
		if len(itr.buf.fields) == 0 {
			if len(itr.mms) == 0 {
				return nil, nil
			}

			itr.buf.mm = itr.mms[0]
			mf := itr.sh.engine.MeasurementFields(itr.buf.mm.Name)
			if mf != nil {
				fset := mf.FieldSet()
				if len(fset) == 0 {
					itr.mms = itr.mms[1:]
					continue
				}

				keys := make([]string, 0, len(fset))
				for k := range fset {
					keys = append(keys, k)
				}
				sort.Strings(keys)

				itr.buf.fields = make([]Field, len(keys))
				for i, name := range keys {
					itr.buf.fields[i] = Field{Name: name, Type: fset[name]}
				}
			}
			itr.mms = itr.mms[1:]
			continue
		}

		// Return next key.
		field := itr.buf.fields[0]
		p := &influxql.FloatPoint{
			Name: itr.buf.mm.Name,
			Aux:  []interface{}{field.Name, field.Type.String()},
		}
		itr.buf.fields = itr.buf.fields[1:]

		return p, nil
	}
}

// MeasurementIterator represents a string iterator that emits all measurement names in a shard.
// 获取一个 shard 中 measurement 名字的迭代器
type MeasurementIterator struct {
	mms Measurements // 保存用于迭代的 measurement 数据
}

// NewMeasurementIterator returns a new instance of MeasurementIterator.
func NewMeasurementIterator(dbi *DatabaseIndex, opt influxql.IteratorOptions) (*MeasurementIterator, error) {
	itr := &MeasurementIterator{}

	// Retrieve measurements from shard. Filter if condition specified.
	// 根据过滤条件取所有或者部分 measurement 的数据
	if opt.Condition == nil {
		itr.mms = dbi.Measurements()
	} else {
		mms, _, err := dbi.measurementsByExpr(opt.Condition)
		if err != nil {
			return nil, err
		}
		itr.mms = mms
	}

	// Sort measurements by name.
	sort.Sort(itr.mms)

	return itr, nil
}

// Stats returns stats about the points processed.
func (itr *MeasurementIterator) Stats() influxql.IteratorStats { return influxql.IteratorStats{} }

// Close closes the iterator.
func (itr *MeasurementIterator) Close() error { return nil }

// Next emits the next measurement name.
func (itr *MeasurementIterator) Next() (*influxql.FloatPoint, error) {
	if len(itr.mms) == 0 {
		return nil, nil
	}
	// 从数组中取出一个
	mm := itr.mms[0]
	itr.mms = itr.mms[1:]
	return &influxql.FloatPoint{
		Name: "measurements",
		Aux:  []interface{}{mm.Name},
	}, nil
}

// seriesIterator emits series ids.
// series 的迭代器
type seriesIterator struct {
	mms  Measurements
	keys struct {
		buf []string
		i   int
	}

	point influxql.FloatPoint // reusable point
	opt   influxql.IteratorOptions
}

// NewSeriesIterator returns a new instance of SeriesIterator.
func NewSeriesIterator(sh *Shard, opt influxql.IteratorOptions) (influxql.Iterator, error) {
	// Only equality operators are allowed.
	// 判断过滤表达式中的运算符是否符合要求
	var err error
	influxql.WalkFunc(opt.Condition, func(n influxql.Node) {
		switch n := n.(type) {
		case *influxql.BinaryExpr:
			switch n.Op {
			case influxql.EQ, influxql.NEQ, influxql.EQREGEX, influxql.NEQREGEX,
				influxql.OR, influxql.AND:
			default:
				err = errors.New("invalid tag comparison operator")
			}
		}
	})
	if err != nil {
		return nil, err
	}

	// Read and sort all measurements.
	// 获取所有 measurement 的数据，其中包括了 series
	mms := sh.index.Measurements()
	sort.Sort(mms)

	return &seriesIterator{
		mms: mms,
		point: influxql.FloatPoint{
			Aux: make([]interface{}, len(opt.Aux)),
		},
		opt: opt,
	}, nil
}

// Stats returns stats about the points processed.
func (itr *seriesIterator) Stats() influxql.IteratorStats { return influxql.IteratorStats{} }

// Close closes the iterator.
func (itr *seriesIterator) Close() error { return nil }

// Next emits the next point in the iterator.
func (itr *seriesIterator) Next() (*influxql.FloatPoint, error) {
	for {
		// Load next measurement's keys if there are no more remaining.
		// 遍历每一个 measurement 中的所有 seriesKey
		if itr.keys.i >= len(itr.keys.buf) {
			if err := itr.nextKeys(); err != nil {
				return nil, err
			}
			if len(itr.keys.buf) == 0 {
				return nil, nil
			}
		}

		// Read the next key.
		key := itr.keys.buf[itr.keys.i]
		itr.keys.i++

		// Write auxiliary fields.
		// 例如 SHOW SERIES 这样的查询语句，seriesKey 被放在 "key" 这个辅助列中返回
		for i, f := range itr.opt.Aux {
			switch f.Val {
			case "key":
				itr.point.Aux[i] = key
			}
		}
		return &itr.point, nil
	}
}

// nextKeys reads all keys for the next measurement.
// 获取下一个 measurement 的所有 keys 信息
func (itr *seriesIterator) nextKeys() error {
	for {
		// Ensure previous keys are cleared out.
		itr.keys.i, itr.keys.buf = 0, itr.keys.buf[:0]

		// Read next measurement.
		if len(itr.mms) == 0 {
			return nil
		}
		mm := itr.mms[0]
		itr.mms = itr.mms[1:]

		// Read all series keys.
		ids, err := mm.seriesIDsAllOrByExpr(itr.opt.Condition)
		if err != nil {
			return err
		} else if len(ids) == 0 {
			continue
		}
		itr.keys.buf = mm.AppendSeriesKeysByID(itr.keys.buf, ids)
		sort.Strings(itr.keys.buf)

		return nil
	}
}

// NewTagKeysIterator returns a new instance of TagKeysIterator.
func NewTagKeysIterator(sh *Shard, opt influxql.IteratorOptions) (influxql.Iterator, error) {
	fn := func(m *Measurement) []string {
		return m.TagKeys()
	}
	return newMeasurementKeysIterator(sh, fn, opt)
}

// tagValuesIterator emits key/tag values
// 指定 tagk 的所有 tagv 的查询迭代器
type tagValuesIterator struct {
	series []*Series // remaining series
	keys   []string  // tag keys to select from a series
	fields []string  // fields to emit (key or value)
	buf    struct {
		s    *Series  // current series
		keys []string // current tag's keys
	}
}

// NewTagValuesIterator returns a new instance of TagValuesIterator.
func NewTagValuesIterator(sh *Shard, opt influxql.IteratorOptions) (influxql.Iterator, error) {
	if opt.Condition == nil {
		return nil, errors.New("a condition is required")
	}

	measurementExpr := influxql.CloneExpr(opt.Condition)
	measurementExpr = influxql.Reduce(influxql.RewriteExpr(measurementExpr, func(e influxql.Expr) influxql.Expr {
		switch e := e.(type) {
		case *influxql.BinaryExpr:
			switch e.Op {
			case influxql.EQ, influxql.NEQ, influxql.EQREGEX, influxql.NEQREGEX:
				tag, ok := e.LHS.(*influxql.VarRef)
				if !ok || tag.Val != "_name" {
					return nil
				}
			}
		}
		return e
	}), nil)

	mms, ok, err := sh.index.measurementsByExpr(measurementExpr)
	if err != nil {
		return nil, err
	} else if !ok {
		mms = sh.index.Measurements()
		sort.Sort(mms)
	}

	// If there are no measurements, return immediately.
	if len(mms) == 0 {
		return &tagValuesIterator{}, nil
	}

	filterExpr := influxql.CloneExpr(opt.Condition)
	filterExpr = influxql.Reduce(influxql.RewriteExpr(filterExpr, func(e influxql.Expr) influxql.Expr {
		switch e := e.(type) {
		case *influxql.BinaryExpr:
			switch e.Op {
			case influxql.EQ, influxql.NEQ, influxql.EQREGEX, influxql.NEQREGEX:
				tag, ok := e.LHS.(*influxql.VarRef)
				if !ok || strings.HasPrefix(tag.Val, "_") {
					return nil
				}
			}
		}
		return e
	}), nil)

	var series []*Series
	keys := newStringSet()
	for _, mm := range mms {
		ss, ok, err := mm.TagKeysByExpr(opt.Condition)
		if err != nil {
			return nil, err
		} else if !ok {
			keys.add(mm.TagKeys()...)
		} else {
			keys = keys.union(ss)
		}

		ids, err := mm.seriesIDsAllOrByExpr(filterExpr)
		if err != nil {
			return nil, err
		}

		for _, id := range ids {
			series = append(series, mm.SeriesByID(id))
		}
	}

	return &tagValuesIterator{
		series: series,
		keys:   keys.list(),
		fields: influxql.VarRefs(opt.Aux).Strings(),
	}, nil
}

// Stats returns stats about the points processed.
func (itr *tagValuesIterator) Stats() influxql.IteratorStats { return influxql.IteratorStats{} }

// Close closes the iterator.
func (itr *tagValuesIterator) Close() error { return nil }

// Next emits the next point in the iterator.
func (itr *tagValuesIterator) Next() (*influxql.FloatPoint, error) {
	for {
		// If there are no more values then move to the next key.
		if len(itr.buf.keys) == 0 {
			if len(itr.series) == 0 {
				return nil, nil
			}

			itr.buf.s = itr.series[0]
			itr.buf.keys = itr.keys
			itr.series = itr.series[1:]
			continue
		}

		key := itr.buf.keys[0]
		value, ok := itr.buf.s.Tags[key]
		if !ok {
			itr.buf.keys = itr.buf.keys[1:]
			continue
		}

		// Prepare auxiliary fields.
		auxFields := make([]interface{}, len(itr.fields))
		for i, f := range itr.fields {
			switch f {
			case "_tagKey":
				auxFields[i] = key
			case "value":
				auxFields[i] = value
			}
		}

		// Return next key.
		p := &influxql.FloatPoint{
			Name: itr.buf.s.measurement.Name,
			Aux:  auxFields,
		}
		itr.buf.keys = itr.buf.keys[1:]

		return p, nil
	}
}

// measurementKeyFunc is the function called by measurementKeysIterator.
type measurementKeyFunc func(m *Measurement) []string

func newMeasurementKeysIterator(sh *Shard, fn measurementKeyFunc, opt influxql.IteratorOptions) (*measurementKeysIterator, error) {
	itr := &measurementKeysIterator{fn: fn}

	// Retrieve measurements from shard. Filter if condition specified.
	if opt.Condition == nil {
		itr.mms = sh.index.Measurements()
	} else {
		mms, _, err := sh.index.measurementsByExpr(opt.Condition)
		if err != nil {
			return nil, err
		}
		itr.mms = mms
	}

	// Sort measurements by name.
	sort.Sort(itr.mms)

	return itr, nil
}

// measurementKeysIterator iterates over measurements and gets keys from each measurement.
// 遍历 measurement 获取其中的 key 的迭代器
type measurementKeysIterator struct {
	mms Measurements // remaining measurements
	buf struct {
		mm   *Measurement // current measurement
		keys []string     // current measurement's keys
	}
	fn measurementKeyFunc
}

// Stats returns stats about the points processed.
func (itr *measurementKeysIterator) Stats() influxql.IteratorStats { return influxql.IteratorStats{} }

// Close closes the iterator.
func (itr *measurementKeysIterator) Close() error { return nil }

// Next emits the next tag key name.
func (itr *measurementKeysIterator) Next() (*influxql.FloatPoint, error) {
	for {
		// If there are no more keys then move to the next measurements.
		if len(itr.buf.keys) == 0 {
			if len(itr.mms) == 0 {
				return nil, nil
			}

			itr.buf.mm = itr.mms[0]
			itr.buf.keys = itr.fn(itr.buf.mm)
			itr.mms = itr.mms[1:]
			continue
		}

		// Return next key.
		p := &influxql.FloatPoint{
			Name: itr.buf.mm.Name,
			Aux:  []interface{}{itr.buf.keys[0]},
		}
		itr.buf.keys = itr.buf.keys[1:]

		return p, nil
	}
}