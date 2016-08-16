package influxql

import (
	"fmt"
	"time"

	"github.com/influxdata/influxdb/models"
)

// Emitter groups values together by name,
type Emitter struct {
	buf       []Point		// 缓存从每个迭代器中获取到的数据，和 itrs 一一对应
	itrs      []Iterator	// 获取数据的迭代器，每一个要返回的 value 有一个数据迭代器
	ascending bool			// 数据是否递增
	chunkSize int

	tags Tags
	row  *models.Row		// 一行数据

	// The columns to attach to each row.
	Columns []string		// 要展示的列

	// Removes the "time" column from output.
	// Used for meta queries where time does not apply.
	OmitTime bool			// 是否去除 time 列的展示
}

// NewEmitter returns a new instance of Emitter that pulls from itrs.
func NewEmitter(itrs []Iterator, ascending bool, chunkSize int) *Emitter {
	return &Emitter{
		buf:       make([]Point, len(itrs)),
		itrs:      itrs,
		ascending: ascending,
		chunkSize: chunkSize,
	}
}

// Close closes the underlying iterators.
func (e *Emitter) Close() error {
	return Iterators(e.itrs).Close()
}

// Emit returns the next row from the iterators.
// 按照顺序获取一行数据
func (e *Emitter) Emit() (row *models.Row, _ error) {
	// Immediately end emission if there are no iterators.
	if len(e.itrs) == 0 {
		return nil, nil
	}

	// Continually read from iterators until they are exhausted.
	for {
		// Fill buffer. Return row if no more points remain.
		// 从多个迭代器中依次取出一条 Point 放入 e.buf 中，之后按照排序的规则按照依次返回
		t, name, tags, err := e.loadBuf()
		if err != nil {
			return nil, err
		} else if t == ZeroTime {
			// 没有任何数据的时候，loadBuf 返回 ZeroTime
			row = e.row
			e.row = nil
			return row, nil
		}

		// Read next set of values from all iterators at a given time/name/tags.
		// If no values are returned then return row.
		values := e.readAt(t, name, tags)
		if values == nil {
			row = e.row
			e.row = nil
			return row, nil
		}

		// If there's no row yet then create one.
		// If the name and tags match the existing row, append to that row if
		// the number of values doesn't exceed the chunk size.
		// Otherwise return existing row and add values to next emitted row.
		if e.row == nil {
			e.createRow(name, tags, values)
		} else if e.row.Name == name && e.tags.Equals(&tags) && (e.chunkSize <= 0 || len(e.row.Values) < e.chunkSize) {
			e.row.Values = append(e.row.Values, values)
		} else {
			row = e.row
			e.createRow(name, tags, values)
			return row, nil
		}
	}
}

// loadBuf reads in points into empty buffer slots.
// Returns the next time/name/tags to emit for.
// 从多个迭代器中依次取出一条 Point 放入 e.buf 中，之后按照排序的规则按照依次返回
func (e *Emitter) loadBuf() (t int64, name string, tags Tags, err error) {
	t = ZeroTime

	for i := range e.itrs {
		// Load buffer, if empty.
		// 如果缓存为空，从迭代器获取数据
		if e.buf[i] == nil {
			e.buf[i], err = e.readIterator(e.itrs[i])
			if err != nil {
				break
			}
		}

		// Skip if buffer is empty.
		// 这里如果仍然为 nil 说明迭代器中已经没有数据，跳过
		p := e.buf[i]
		if p == nil {
			continue
		}
		itrTime, itrName, itrTags := p.time(), p.name(), p.tags()

		// Initialize range values if not set.
		if t == ZeroTime {
			t, name, tags = itrTime, itrName, itrTags
			continue
		}

		// Update range values if lower and emitter is in time ascending order.
		if e.ascending {
			if (itrTime < t) || (itrTime == t && itrName < name) || (itrTime == t && itrName == name && itrTags.ID() < tags.ID()) {
				t, name, tags = itrTime, itrName, itrTags
			}
			continue
		}

		// Update range values if higher and emitter is in time descending order.
		if (itrTime > t) || (itrTime == t && itrName > name) || (itrTime == t && itrName == name && itrTags.ID() > tags.ID()) {
			t, name, tags = itrTime, itrName, itrTags
		}
	}

	return
}

// createRow creates a new row attached to the emitter.
func (e *Emitter) createRow(name string, tags Tags, values []interface{}) {
	e.tags = tags
	e.row = &models.Row{
		Name:    name,
		Tags:    tags.KeyValues(),
		Columns: e.Columns,
		Values:  [][]interface{}{values},
	}
}

// readAt returns the next slice of values from the iterators at time/name/tags.
// Returns nil values once the iterators are exhausted.
func (e *Emitter) readAt(t int64, name string, tags Tags) []interface{} {
	// If time is included then move colums over by one.
	offset := 1
	if e.OmitTime {
		offset = 0
	}

	values := make([]interface{}, len(e.itrs)+offset)
	// 如果要显示时间，第一列就是时间
	if !e.OmitTime {
		values[0] = time.Unix(0, t).UTC()
	}

	for i, p := range e.buf {
		// Skip if buffer is empty.
		// 如果缓存中的 Point 为空，设置 value 为 nil
		if p == nil {
			values[i+offset] = nil
			continue
		}

		// Skip point if it doesn't match time/name/tags.
		pTags := p.tags()
		if p.time() != t || p.name() != name || !pTags.Equals(&tags) {
			values[i+offset] = nil
			continue
		}

		// Read point value.
		// 将 point 的 value 写入
		values[i+offset] = p.value()

		// Clear buffer.
		e.buf[i] = nil
	}

	return values
}

// readIterator reads the next point from itr.
// 从迭代器中获取下一条数据
func (e *Emitter) readIterator(itr Iterator) (Point, error) {
	if itr == nil {
		return nil, nil
	}

	switch itr := itr.(type) {
	case FloatIterator:
		if p, err := itr.Next(); err != nil {
			return nil, err
		} else if p != nil {
			return p, nil
		}
	case IntegerIterator:
		if p, err := itr.Next(); err != nil {
			return nil, err
		} else if p != nil {
			return p, nil
		}
	case StringIterator:
		if p, err := itr.Next(); err != nil {
			return nil, err
		} else if p != nil {
			return p, nil
		}
	case BooleanIterator:
		if p, err := itr.Next(); err != nil {
			return nil, err
		} else if p != nil {
			return p, nil
		}
	default:
		panic(fmt.Sprintf("unsupported iterator: %T", itr))
	}
	return nil, nil
}
