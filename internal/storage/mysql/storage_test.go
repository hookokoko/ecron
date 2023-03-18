package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/gotomicro/ecron/internal/storage"
	"github.com/gotomicro/ecron/internal/task"
	"github.com/gotomicro/eorm"
	"github.com/stretchr/testify/assert"
)

// 增删改查
func Test_StorageTaskCURD(t *testing.T) {
	s, err := NewMysqlStorage("root:@tcp(localhost:3306)/ecron")
	assert.Nil(t, err)

	// 添加任务
	addTask := &task.Task{Config: task.Config{
		Name:       "Origin Task",
		Cron:       "*/5 * * * * * *", // every 5s
		Type:       task.TypeHTTP,
		Parameters: `{"url": "http://www.baidu.com", "body": "{\"key\": \"value\"}", "timeout": 30}`,
	}}
	tId, err := s.Add(context.TODO(), addTask)
	assert.Nil(t, err)
	assert.NotEmpty(t, tId)

	getTask, err := s.Get(context.TODO(), tId)
	assert.Nil(t, err)
	addTask.TaskId = tId
	assert.Equal(t, addTask, getTask)

	// 更新任务
	updateTask := &task.Task{
		Config: task.Config{
			Name:       "Update Task",
			Cron:       "*/20 * * * * * *", // every 5s
			Type:       task.TypeHTTP,
			Parameters: `{"url": "http://www.sina.com", "body": "{\"key\": \"value\"}", "timeout": 30}`,
		},
		TaskId: tId,
	}
	err = s.Update(context.TODO(), updateTask)
	assert.Nil(t, err)
	getUpdateTask, err := s.Get(context.TODO(), tId)
	assert.Nil(t, err)
	addTask.TaskId = tId
	assert.Equal(t, updateTask, getUpdateTask)

	// 删除任务
	err = s.Delete(context.TODO(), tId)
	assert.Nil(t, err)

	getDelTask, err := s.Get(context.TODO(), tId)
	assert.Nil(t, err)
	assert.Nil(t, getDelTask)

	// 停止storage
	err = s.Stop(context.TODO())
	assert.Nil(t, err)

	_ = eorm.NewDeleter[StorageInfo](s.db).From(&StorageInfo{}).Where(eorm.C("Id").EQ(s.storageId)).Exec(context.TODO())
}

// 测试场景-单个storage抢占
//
//	单个storage，确定一下对应条件的任务能否被抢占
//		- 任务由于负载问题被放弃
//		- 任务由于处于刚创建的状态
//		- 任务处于抢占状态，几个续约周期内没有续约
func TestStorage_SinglePreempt(t *testing.T) {
	var (
		st    *Storage
		tasks []int64
		db    *eorm.DB
	)

	db, err := eorm.Open("mysql", "root:@tcp(localhost:3306)/ecron")
	assert.Nil(t, err)

	testCases := []struct {
		name                 string
		wantType             storage.EventType
		before               func()
		after                func()
		wantOccupierPayload  int64
		wantCandidatePayload int64
		wantOccupierId       uint32
		wantCandidateId      uint32
	}{
		{
			name: "抢占处于创建的状态的任务",
			before: func() {
				st, _ = newMysqlStorage(db)
				for i := 0; i < 2; i++ {
					tId, err := eorm.NewInserter[TaskInfo](db).Values(&TaskInfo{
						Name:            "test task",
						Cron:            "*/5 * * * * * *",
						SchedulerStatus: storage.EventTypeCreated,
						CreateTime:      time.Now().UnixMilli(),
						UpdateTime:      time.Now().UnixMilli(),
					}).Exec(context.TODO()).LastInsertId()
					if err == nil {
						tasks = append(tasks, tId)
					}
				}
			},
			after: func() {
				for _, tId := range tasks {
					_ = eorm.NewDeleter[TaskInfo](st.db).From(&TaskInfo{}).Where(eorm.C("Id").EQ(tId)).Exec(context.TODO())
				}
				_ = eorm.NewDeleter[StorageInfo](db).From(&StorageInfo{}).Where(eorm.C("Id").EQ(st.storageId)).Exec(context.TODO())
			},
			wantType:            storage.EventTypePreempted,
			wantOccupierPayload: 2,
		},
		{
			name: "抢占由于负载问题被放弃的任务",
			before: func() {
				st, _ = newMysqlStorage(db)
				for i := 0; i < 2; i++ {
					tId, err := eorm.NewInserter[TaskInfo](db).Values(&TaskInfo{
						Name:            "test task",
						SchedulerStatus: storage.EventTypeDiscarded,
						CandidateId:     st.storageId,
						CreateTime:      time.Now().UnixMilli(),
						UpdateTime:      time.Now().UnixMilli(),
					}).Exec(context.TODO()).LastInsertId()
					if err == nil {
						tasks = append(tasks, tId)
					}
				}
			},
			after: func() {
				for _, tId := range tasks {
					_ = eorm.NewDeleter[TaskInfo](st.db).From(&TaskInfo{}).Where(eorm.C("Id").EQ(tId)).Exec(context.TODO())
				}
				_ = eorm.NewDeleter[StorageInfo](db).From(&StorageInfo{}).Where(eorm.C("Id").EQ(st.storageId)).Exec(context.TODO())
			},
			wantType:            storage.EventTypePreempted,
			wantOccupierPayload: 2,
		},
		{
			name: "抢占处于抢占状态但是在续约周期内没有续约的任务",
			before: func() {
				retry := &storage.RefreshIntervalRetry{Interval: time.Second, Max: 3}
				st, _ = newMysqlStorage(db, WithRefreshRetry(retry))
				for i := 0; i < 2; i++ {
					tId, err := eorm.NewInserter[TaskInfo](db).Values(&TaskInfo{
						Name:            "test task",
						SchedulerStatus: storage.EventTypePreempted,
						OccupierId:      st.storageId,
						// 模拟续约超时
						UpdateTime: time.Now().Unix() - (retry.GetMaxRetry()+1000)*int64(retry.Interval.Seconds()),
					}).Exec(context.TODO()).LastInsertId()
					if err == nil {
						tasks = append(tasks, tId)
					}
				}
			},
			after: func() {
				for _, tId := range tasks {
					_ = eorm.NewDeleter[TaskInfo](st.db).From(&TaskInfo{}).Where(eorm.C("Id").EQ(tId)).Exec(context.TODO())
				}
				_ = eorm.NewDeleter[StorageInfo](db).From(&StorageInfo{}).Where(eorm.C("Id").EQ(st.storageId)).Exec(context.TODO())

			},
			wantType:            storage.EventTypePreempted,
			wantOccupierPayload: 2,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.before()
			go st.preempted(context.TODO())
			taskIds := make([]int64, 0, 2)
		LOOP:
			for {
				select {
				case e := <-st.events:
					assert.Equal(t, tc.wantType, e.Type)
					taskIds = append(taskIds, e.Task.TaskId)
					if len(taskIds) == 2 {
						break LOOP
					}
				}
			}
			for _, tId := range taskIds {
				taskDb := getDbTask(db, tId)
				occupierStorageInfo := getStorageInfo(st, taskDb.OccupierId)
				assert.Equal(t, tc.wantOccupierPayload, occupierStorageInfo.Payload)
				assert.Equal(t, st.storageId, taskDb.OccupierId)
			}
			tc.after()
		})
	}
}

// 任务续约。eorm.openDB是私有，暂无法覆盖所有场景
func TestStorage_Refresh(t *testing.T) {
	db, err := eorm.Open("mysql", "root:@tcp(localhost:3306)/ecron")
	assert.Nil(t, err)

	var taskId int64
	s, _ := newMysqlStorage(db, WithRefreshInterval(5*time.Second))
	testCases := []struct {
		name      string
		before    func()
		after     func()
		wantEpoch int64
	}{
		{
			name: "一次续约即成功",
			before: func() {
				taskId, _ = eorm.NewInserter[TaskInfo](db).Values(&TaskInfo{
					Name:            "test task",
					SchedulerStatus: storage.EventTypePreempted,
					OccupierId:      s.storageId,
					CreateTime:      time.Now().UnixMilli(),
					UpdateTime:      time.Now().UnixMilli(),
				}).Exec(context.TODO()).LastInsertId()
			},
			after: func() {
				_ = eorm.NewDeleter[TaskInfo](s.db).From(&TaskInfo{}).Where(eorm.C("Id").EQ(taskId)).Exec(context.TODO())
				_ = eorm.NewDeleter[StorageInfo](db).From(&StorageInfo{}).Where(eorm.C("Id").EQ(s.storageId)).Exec(context.TODO())
			},
			wantEpoch: 1,
		},
		//{
		//	name: "多次续约才成功",
		//},
		//{
		//	name: "续约都失败",
		//},
		//{
		//	name: "续约超时",
		//},
		//{
		//	name: "续约时，任务状态由抢占变为非抢占状态",
		//},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.before()
			tskBefore := getDbTask(db, taskId)
			assert.Equal(t, taskId, tskBefore.Id)
			s.refresh(context.TODO(), tskBefore.Id, tskBefore.Epoch, tskBefore.CandidateId)
			tskAfter := getDbTask(db, taskId)
			assert.Equal(t, tskAfter.Epoch, tc.wantEpoch)
			assert.Equal(t, s.refreshRetry.GetCntRetry(), int64(0))
			tc.after()
		})
	}
}

// 任务负载更新(任务均衡) 。确保当前storage，是否按照需求要更新到遍历的任务中
func TestStorage_Lookup(t *testing.T) {
	db, err := eorm.Open("mysql", "root:@tcp(localhost:3306)/ecron")
	assert.Nil(t, err)
	s1, _ := newMysqlStorage(db) // task占有者storage
	s2, _ := newMysqlStorage(db)
	s, _ := newMysqlStorage(db) // 当前storage

	_ = eorm.NewInserter[TaskInfo](db).Values(&TaskInfo{
		Id:              10001,
		SchedulerStatus: storage.EventTypePreempted,
		OccupierId:      s.storageId,
		CreateTime:      time.Now().UnixMilli(),
		UpdateTime:      time.Now().UnixMilli(),
	}).Exec(context.TODO()).Err()

	testCases := []struct {
		name            string
		before          func()
		after           func()
		wantOccupierId  int64
		wantCandidateId int64
	}{
		{
			name: "占有者就是当前storage(跳过本次balance)",
			before: func() {
				s.payLoad = 9
				_ = eorm.NewUpdater[StorageInfo](s.db).Update(&StorageInfo{Payload: 9}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[StorageInfo](s1.db).Update(&StorageInfo{Payload: 1}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s1.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[StorageInfo](s2.db).Update(&StorageInfo{Payload: 2}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s2.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[TaskInfo](db).Update(&TaskInfo{
					CandidateId: 0,
					OccupierId:  s1.storageId,
					UpdateTime:  time.Now().UnixMilli(),
				}).Set(eorm.Columns("CandidateId", "OccupierId", "UpdateTime")).Where(eorm.C("Id").EQ("10001")).Exec(context.TODO()).Err()
			},
			after: func() {
			},
			wantCandidateId: 0, // 无候选者
			wantOccupierId:  s.storageId,
		},
		{
			name: "task无候选者,当前节点比占有节点负载大",
			before: func() {
				s.payLoad = 9
				_ = eorm.NewUpdater[StorageInfo](s.db).Update(&StorageInfo{Payload: 9}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[StorageInfo](s1.db).Update(&StorageInfo{Payload: 1}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s1.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[StorageInfo](s2.db).Update(&StorageInfo{Payload: 2}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s2.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[TaskInfo](db).Update(&TaskInfo{
					CandidateId: 0,
					OccupierId:  s1.storageId,
					UpdateTime:  time.Now().UnixMilli(),
				}).Set(eorm.Columns("CandidateId", "OccupierId", "UpdateTime")).Where(eorm.C("Id").EQ("10001")).Exec(context.TODO()).Err()
			},
			after:           func() {},
			wantCandidateId: 0, // 无候选者
			wantOccupierId:  s1.storageId,
		},
		{
			name: "task无候选者,待选节点比占有节点负载小",
			before: func() {
				s.payLoad = 3
				_ = eorm.NewUpdater[StorageInfo](s.db).Update(&StorageInfo{Payload: 3}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[StorageInfo](s1.db).Update(&StorageInfo{Payload: 9}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s1.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[StorageInfo](s2.db).Update(&StorageInfo{Payload: 8}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s2.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[TaskInfo](db).Update(&TaskInfo{
					CandidateId: 0,
					OccupierId:  s1.storageId,
					UpdateTime:  time.Now().UnixMilli(),
				}).Set(eorm.Columns("CandidateId", "OccupierId", "OccupierId", "UpdateTime")).Where(eorm.C("Id").EQ("10001")).Exec(context.TODO()).Err()
			},
			after:           func() {},
			wantCandidateId: s.storageId,
			wantOccupierId:  s1.storageId,
		},
		{
			name: "task有候选者,占有者和候选者节点都比当前节点负载小(不替换候选者为当前storage)",
			before: func() {
				s.payLoad = 9
				_ = eorm.NewUpdater[StorageInfo](s.db).Update(&StorageInfo{Payload: 9}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[StorageInfo](s1.db).Update(&StorageInfo{Payload: 3}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s1.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[StorageInfo](s2.db).Update(&StorageInfo{Payload: 1}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s2.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[TaskInfo](db).Update(&TaskInfo{
					CandidateId: s2.storageId,
					OccupierId:  s1.storageId,
					UpdateTime:  time.Now().UnixMilli(),
				}).Set(eorm.Columns("CandidateId", "OccupierId", "UpdateTime")).Where(eorm.C("Id").EQ("10001")).Exec(context.TODO()).Err()
			},
			after:           func() {},
			wantCandidateId: s2.storageId,
			wantOccupierId:  s1.storageId,
		},
		{
			name: "task有候选者,占有者和候选者节点都比当前节点负载大(替换候选者为当前storage)",
			before: func() {
				s.payLoad = 3
				_ = eorm.NewUpdater[StorageInfo](s.db).Update(&StorageInfo{Payload: 3}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[StorageInfo](s1.db).Update(&StorageInfo{Payload: 9}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s1.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[StorageInfo](s2.db).Update(&StorageInfo{Payload: 4}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s2.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[TaskInfo](db).Update(&TaskInfo{
					CandidateId: s2.storageId,
					OccupierId:  s1.storageId,
					UpdateTime:  time.Now().UnixMilli(),
				}).Set(eorm.Columns("CandidateId", "OccupierId", "UpdateTime")).Where(eorm.C("Id").EQ("10001")).Exec(context.TODO()).Err()
			},
			after:           func() {},
			wantCandidateId: s.storageId,
			wantOccupierId:  s1.storageId,
		},
		{
			name: "task有候选者,占有者比当前节点负载小，候选者节点负载比当前节点大(不用当前storage进行候选者替换)",
			before: func() {
				s.payLoad = 3
				_ = eorm.NewUpdater[StorageInfo](s.db).Update(&StorageInfo{Payload: 4}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[StorageInfo](s1.db).Update(&StorageInfo{Payload: 3}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s1.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[StorageInfo](s2.db).Update(&StorageInfo{Payload: 9}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s2.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[TaskInfo](db).Update(&TaskInfo{
					CandidateId: s2.storageId,
					OccupierId:  s1.storageId,
					UpdateTime:  time.Now().UnixMilli(),
				}).Set(eorm.Columns("CandidateId", "OccupierId", "UpdateTime")).Where(eorm.C("Id").EQ("10001")).Exec(context.TODO()).Err()
			},
			after:           func() {},
			wantCandidateId: s2.storageId,
			wantOccupierId:  s1.storageId,
		},
		{
			name: "task有候选者,占有者比当前节点负载大，候选者节点负载比当前节点小(不用当前storage进行候选者替换)",
			before: func() {
				s.payLoad = 4
				_ = eorm.NewUpdater[StorageInfo](s.db).Update(&StorageInfo{Payload: 4}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[StorageInfo](s1.db).Update(&StorageInfo{Payload: 9}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s1.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[StorageInfo](s2.db).Update(&StorageInfo{Payload: 3}).Set(eorm.Columns("Payload")).Where(eorm.C("Id").EQ(s2.storageId)).Exec(context.TODO()).Err()
				_ = eorm.NewUpdater[TaskInfo](db).Update(&TaskInfo{
					CandidateId: s2.storageId,
					OccupierId:  s1.storageId,
					UpdateTime:  time.Now().UnixMilli(),
				}).Set(eorm.Columns("CandidateId", "OccupierId", "UpdateTime")).Where(eorm.C("Id").EQ("10001")).Exec(context.TODO()).Err()
			},
			after:           func() {},
			wantCandidateId: s2.storageId,
			wantOccupierId:  s1.storageId,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.before()
			s.lookup(context.TODO())
			tsk := getDbTask(db, 10001)
			assert.Equal(t, tc.wantCandidateId, tsk.CandidateId)
			tc.after()
		})
	}
	_ = eorm.NewDeleter[StorageInfo](db).From(&StorageInfo{}).Where(eorm.C("Id").EQ(s.storageId)).Exec(context.TODO())
	_ = eorm.NewDeleter[StorageInfo](db).From(&StorageInfo{}).Where(eorm.C("Id").EQ(s1.storageId)).Exec(context.TODO())
	_ = eorm.NewDeleter[StorageInfo](db).From(&StorageInfo{}).Where(eorm.C("Id").EQ(s2.storageId)).Exec(context.TODO())
	_ = eorm.NewDeleter[TaskInfo](db).From(&TaskInfo{}).Where(eorm.C("Id").EQ(10001)).Exec(context.TODO())
}

func getStorageInfo(s *Storage, storageId int64) *StorageInfo {
	ts, err := eorm.NewSelector[StorageInfo](s.db).Select(eorm.C("Id"), eorm.C("Payload")).
		From(eorm.TableOf(&StorageInfo{}, "t1")).
		Where(eorm.C("Id").EQ(storageId)).
		Get(context.TODO())
	if ts == nil || err != nil {
		return &StorageInfo{}
	}
	return ts
}

func getDbTask(db *eorm.DB, taskId int64) *TaskInfo {
	ts, _ := eorm.NewSelector[TaskInfo](db).From(eorm.TableOf(&TaskInfo{}, "t1")).
		Where(eorm.C("Id").EQ(taskId)).
		Get(context.TODO())
	return ts
}
