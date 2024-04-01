package shardkv

import (
	"6.824/labgob"
	"6.824/labrpc"
	"6.824/raft"
	"6.824/shardctrler"
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type ShardKV struct {
	mu      sync.RWMutex
	dead    int32
	rf      *raft.Raft
	applyCh chan raft.ApplyMsg

	makeEnd func(string) *labrpc.ClientEnd
	gid     int
	sc      *shardctrler.Clerk

	maxRaftState int
	lastApplied  int

	lastConfig    shardctrler.Config
	currentConfig shardctrler.Config

	stateMachines  map[int]*Shard
	lastOperations map[int64]OperationContext
	notifyChans    map[int]chan *CommandResponse
}

func StartServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxRaftState int, gid int, ctrlers []*labrpc.ClientEnd, makeEnd func(string) *labrpc.ClientEnd) *ShardKV {
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Command{})
	labgob.Register(CommandRequest{})
	labgob.Register(shardctrler.Config{})
	labgob.Register(ShardOperationResponse{})
	labgob.Register(ShardOperationRequest{})

	applyCh := make(chan raft.ApplyMsg)

	kv := &ShardKV{
		dead:           0,
		rf:             raft.Make(servers, me, persister, applyCh),
		applyCh:        applyCh,
		makeEnd:        makeEnd,
		gid:            gid,
		sc:             shardctrler.MakeClerk(ctrlers),
		lastApplied:    0,
		maxRaftState:   maxRaftState,
		currentConfig:  shardctrler.DefaultConfig(),
		lastConfig:     shardctrler.DefaultConfig(),
		stateMachines:  make(map[int]*Shard),
		lastOperations: make(map[int64]OperationContext),
		notifyChans:    make(map[int]chan *CommandResponse),
	}
	kv.restoreSnapshot(persister.ReadSnapshot())
	// start applier goroutine to apply committed logs to stateMachine
	go kv.applier()
	// start configuration monitor goroutine to fetch latest configuration
	go kv.Monitor(kv.configureAction, ConfigureMonitorTimeout)
	// start migration monitor goroutine to pull related shards
	go kv.Monitor(kv.migrationAction, MigrationMonitorTimeout)
	// start gc monitor goroutine to delete useless shards in remote groups
	go kv.Monitor(kv.gcAction, GCMonitorTimeout)
	// start entry-in-currentTerm monitor goroutine to advance commitIndex by appending empty entries in current term periodically to avoid live locks
	go kv.Monitor(kv.checkEntryInCurrentTermAction, EmptyEntryDetectorTimeout)

	DPrintf("{Node %v}{Group %v} has started", kv.rf.Me(), kv.gid)
	return kv
}

func (kv *ShardKV) restoreSnapshot(snapshot []byte) {
	if snapshot == nil || len(snapshot) == 0 {
		kv.initStateMachines()
		return
	}
	r := bytes.NewBuffer(snapshot)
	d := labgob.NewDecoder(r)
	var stateMachines map[int]*Shard
	var lastOperations map[int64]OperationContext
	var currentConfig shardctrler.Config
	var lastConfig shardctrler.Config
	if d.Decode(&stateMachines) != nil ||
		d.Decode(&lastOperations) != nil ||
		d.Decode(&currentConfig) != nil ||
		d.Decode(&lastConfig) != nil {
		DPrintf("{Node %v}{Group %v} restores snapshot failed", kv.rf.Me(), kv.gid)
	}
	kv.stateMachines, kv.lastOperations, kv.currentConfig, kv.lastConfig = stateMachines, lastOperations, currentConfig, lastConfig
}

func (kv *ShardKV) initStateMachines() {
	for i := 0; i < shardctrler.NShards; i++ {
		if _, ok := kv.stateMachines[i]; !ok {
			kv.stateMachines[i] = NewShard()
		}
	}
}

func (kv *ShardKV) applier() {
	for kv.killed() == false {
		select {
		case message := <-kv.applyCh:
			DPrintf("{Node %v}{Group %v} tries to apply message %v", kv.rf.Me(), kv.gid, message)
			if message.CommandValid {
				kv.mu.Lock()
				if message.CommandIndex <= kv.lastApplied {
					DPrintf("{Node %v}{Group %v} discards outdated message %v because a newer snapshot which lastApplied is %v has been restored", kv.rf.Me(), kv.gid, message, kv.lastApplied)
					kv.mu.Unlock()
					continue
				}
				kv.lastApplied = message.CommandIndex

				var response *CommandResponse
				command := message.Command.(Command)
				switch command.Op {
				case Operation:
					operation := command.Data.(CommandRequest)
					response = kv.applyOperation(&message, &operation)
				case Configuration:
					nextConfig := command.Data.(shardctrler.Config)
					response = kv.applyConfiguration(&nextConfig)
				case InsertShards:
					shardsInfo := command.Data.(ShardOperationResponse)
					response = kv.applyInsertDataToShards(&shardsInfo)
				case DeleteShards:
					shardsInfo := command.Data.(ShardOperationRequest)
					response = kv.applyDeleteShards(&shardsInfo)
				case EmptyEntry:
					response = kv.applyEmptyEntry()
				}
				// only notify related channel for currentTerm's log when node is leader
				if currentTerm, isLeader := kv.rf.GetState(); isLeader && message.CommandTerm == currentTerm {
					ch := kv.getNotifyChan(message.CommandIndex)
					ch <- response
				}
				kv.mu.Unlock()
			}else{
				if message.SnapshotValid {
					kv.mu.Lock()
					if kv.rf.CondInstallSnapshot(message.SnapshotTerm, message.SnapshotIndex, message.Snapshot) {
						kv.restoreSnapshot(message.Snapshot)
						kv.lastApplied = message.SnapshotIndex
					}
					kv.mu.Unlock()
				}else{
					panic(fmt.Sprintf("unexpected Message %v", message))
				}
			}
		}
	}
}

func (kv *ShardKV) Monitor(action func(), timeout time.Duration) {
	for kv.killed() == false {
		if _, isLeader := kv.rf.GetState(); isLeader {
			action()
		}
		time.Sleep(timeout)
	}
}

func (kv *ShardKV) Command(request *CommandRequest, response *CommandResponse) {
	kv.mu.RLock()
	//判重
	if request.Op != OpGet && kv.isDuplicateRequest(request.ClientId, request.CommandId) {
		lastResponse := kv.lastOperations[request.ClientId].LastResponse
		response.Value, response.Err = lastResponse.Value, lastResponse.Err
		kv.mu.RUnlock()
		return
	}
	// 判断该管理该键的分片的状态是否处于可用状态，即serving或者gcing状态
	if !kv.canServe(key2shard(request.Key)) {
		response.Err = ErrWrongGroup
		kv.mu.RUnlock()
		return
	}
	kv.mu.RUnlock()
	kv.Execute(NewOperationCommand(request), response)
}

func (kv *ShardKV) isDuplicateRequest(clientId int64, requestId int64) bool {
	operationContext, ok := kv.lastOperations[clientId]
	return ok && requestId <= operationContext.MaxAppliedCommandId
}

func (kv *ShardKV) canServe(shardID int) bool {
	return kv.currentConfig.Shards[shardID] == kv.gid && (kv.stateMachines[shardID].Status == Serving || kv.stateMachines[shardID].Status == GCing)
}

// 封装execute函数的原因很简单，本身execute的逻辑在command里面就可以完成，但是我们写入的日志有很多不同的结构，
// 而execute这部分是通用的逻辑，虽然只有kv的操作是调用command方法，但是既然这部分是通用的逻辑，不妨提取出来
func (kv *ShardKV) Execute(command Command, response *CommandResponse) {
	// do not hold lock to improve throughput
	// when KVServer holds the lock to take snapshot, underlying raft can still commit raft logs
	index, _, isLeader := kv.rf.Start(command)
	if !isLeader {
		response.Err = ErrWrongLeader
		return
	}
	defer DPrintf("{Node %v}{Group %v} processes Command %v with CommandResponse %v", kv.rf.Me(), kv.gid, command, response)
	kv.mu.Lock()
	ch := kv.getNotifyChan(index)
	kv.mu.Unlock()
	select {
	case result := <-ch:
		response.Value, response.Err = result.Value, result.Err
	case <-time.After(ExecuteTimeout):
		response.Err = ErrTimeout
	}
	go func() {
		kv.mu.Lock()
		delete(kv.notifyChans, index)
		kv.mu.Unlock()
	}()
}

func (kv *ShardKV) getNotifyChan(index int) chan *CommandResponse {
	if _, ok := kv.notifyChans[index]; !ok {
		kv.notifyChans[index] = make(chan *CommandResponse, 1)
	}
	return kv.notifyChans[index]
}

func (kv *ShardKV) applyOperation(message *raft.ApplyMsg, operation *CommandRequest) *CommandResponse {
	var response *CommandResponse
	// raft服务器复制日志的期间也可能发生shard状态的更改，所以需要再次查看分片状态
	shardID := key2shard(operation.Key)
	if kv.canServe(shardID) {
		if operation.Op != OpGet && kv.isDuplicateRequest(operation.ClientId, operation.CommandId) {
			DPrintf("{Node %v}{Group %v} doesn't apply duplicated message %v to stateMachine because maxAppliedCommandId is %v for client %v", kv.rf.Me(), kv.gid, message, kv.lastOperations[operation.ClientId], operation.ClientId)
			return kv.lastOperations[operation.ClientId].LastResponse
		} else {
			response = kv.applyLogToStateMachines(operation, shardID)
			if operation.Op != OpGet {
				kv.lastOperations[operation.ClientId] = OperationContext{operation.CommandId, response}
			}
			return response
		}
	}
	return &CommandResponse{ErrWrongGroup, ""}
}

func (kv *ShardKV) applyLogToStateMachines(operation *CommandRequest, shardID int) *CommandResponse {
	var value string
	var err Err
	switch operation.Op {
	case OpPut:
		err = kv.stateMachines[shardID].Put(operation.Key, operation.Value)
	case OpAppend:
		err = kv.stateMachines[shardID].Append(operation.Key, operation.Value)
	case OpGet:
		value, err = kv.stateMachines[shardID].Get(operation.Key)
	}
	return &CommandResponse{err, value}
}

// 获取关于分片状态的数组
func (kv *ShardKV) getShardStatus() []ShardStatus {
	results := make([]ShardStatus, shardctrler.NShards)
	for i := 0; i < shardctrler.NShards; i++ {
		results[i] = kv.stateMachines[i].Status
	}
	return results
}

func (kv *ShardKV) Kill() {
	DPrintf("{Node %v}{Group %v} has been killed", kv.rf.Me(), kv.gid)
	atomic.StoreInt32(&kv.dead, 1)
	kv.rf.Kill()
}

func (kv *ShardKV) killed() bool {
	return atomic.LoadInt32(&kv.dead) == 1
}

// 一阶段
// 更新插入和移除的分片的状态,将本次配置文件移入改组的shard置为pulling状态，
//而移除的shard置为bepulling(正在被拉取的状态)状态，并且更新本shardserver的config版本号
// 1，配合monitor轮询检测是否有新配置到来
func (kv *ShardKV) configureAction() {
	canPerformNextConfig := true
	kv.mu.RLock()
	// 日志配置文件是一个一个拉取的，需要上一个日志文件配置完成才可以拉去下个配置文件
	// 而如果存在一个分片shard的状态是serving，则证明上一个配置的配置任务还未完成
	for _, shard := range kv.stateMachines {
		if shard.Status != Serving {
			canPerformNextConfig = false
			DPrintf("{Node %v}{Group %v} will not try to fetch latest configuration because shards status are %v when currentConfig is %v", kv.rf.Me(), kv.gid, kv.getShardStatus(), kv.currentConfig)
			break
		}
	}
	currentConfigNum := kv.currentConfig.Num
	kv.mu.RUnlock()
	if canPerformNextConfig {
		nextConfig := kv.sc.Query(currentConfigNum + 1)
		if nextConfig.Num == currentConfigNum+1 {
			DPrintf("{Node %v}{Group %v} fetches latest configuration %v when currentConfigNum is %v", kv.rf.Me(), kv.gid, nextConfig, currentConfigNum)
			kv.Execute(NewConfigurationCommand(&nextConfig), &CommandResponse{})
		}
	}
}

func (kv *ShardKV) applyConfiguration(nextConfig *shardctrler.Config) *CommandResponse {
	if nextConfig.Num == kv.currentConfig.Num+1 {
		DPrintf("{Node %v}{Group %v} updates currentConfig from %v to %v", kv.rf.Me(), kv.gid, kv.currentConfig, nextConfig)
		kv.updateShardStatus(nextConfig)
		kv.lastConfig = kv.currentConfig
		kv.currentConfig = *nextConfig
		return &CommandResponse{OK, ""}
	}
	DPrintf("{Node %v}{Group %v} rejects outdated config %v when currentConfig is %v", kv.rf.Me(), kv.gid, nextConfig, kv.currentConfig)
	return &CommandResponse{ErrOutDated, ""}
}

func (kv *ShardKV) updateShardStatus(nextConfig *shardctrler.Config) {
	for i := 0; i < shardctrler.NShards; i++ {
		if kv.currentConfig.Shards[i] != kv.gid && nextConfig.Shards[i] == kv.gid {
			gid := kv.currentConfig.Shards[i]
			if gid != 0 {
				kv.stateMachines[i].Status = Pulling
			}
		}
		if kv.currentConfig.Shards[i] == kv.gid && nextConfig.Shards[i] != kv.gid {
			gid := nextConfig.Shards[i]
			if gid != 0 {
				kv.stateMachines[i].Status = BePulling
			}
		}
	}
}

//二阶段
// 先查看哪些分片处于pulling状态，分装成map返回，其中key是该shard的旧gid，value是旧gid下的所有状态为pulling的shard
// 将map用于rpc调用从之前的gid中拉取对应的shard数据，拉取至本group下的对应shard
// 需要注意的是，这个状态为pulling的分片是指和本group相关的shard，因为只有新移入本group的分片才会被置为pulling，只关乎自己的
// 注意这里已经更新了config的版本号了，所以旧gid指的是该shard之前的主人
func (kv *ShardKV) migrationAction() {
	kv.mu.RLock()
	// 返回map[gid][shards],可以理解成从哪些gid移入该group的分片
	gid2shardIDs := kv.getShardIDsByStatus(Pulling)
	var wg sync.WaitGroup
	for gid, shardIDs := range gid2shardIDs {
		DPrintf("{Node %v}{Group %v} starts a PullTask to get shards %v from group %v when config is %v", kv.rf.Me(), kv.gid, shardIDs, gid, kv.currentConfig)
		wg.Add(1)
		go func(servers []string, configNum int, shardIDs []int) {
			defer wg.Done()
			pullTaskRequest := ShardOperationRequest{configNum, shardIDs}
			for _, server := range servers {
				var pullTaskResponse ShardOperationResponse
				srv := kv.makeEnd(server)
				// 调用rpc，从旧group拉取对应的分片数据
				if srv.Call("ShardKV.GetShardsData", &pullTaskRequest, &pullTaskResponse) && pullTaskResponse.Err == OK {
					DPrintf("{Node %v}{Group %v} gets a PullTaskResponse %v and tries to commit it when currentConfigNum is %v", kv.rf.Me(), kv.gid, pullTaskResponse, configNum)
					// 将拉取回来的数据插入本group下对应的shard
					kv.Execute(NewInsertShardsCommand(&pullTaskResponse), &CommandResponse{})
				}
			}
		}(kv.lastConfig.Groups[gid], kv.currentConfig.Num, shardIDs)
	}
	kv.mu.RUnlock()
	wg.Wait()
}

func (kv *ShardKV) getShardIDsByStatus(status ShardStatus) map[int][]int {
	gid2shardIDs := make(map[int][]int)
	for i, shard := range kv.stateMachines {
		if shard.Status == status {
			gid := kv.lastConfig.Shards[i]
			if gid != 0 {
				if _, ok := gid2shardIDs[gid]; !ok {
					gid2shardIDs[gid] = make([]int, 0)
				}
				gid2shardIDs[gid] = append(gid2shardIDs[gid], i)
			}
		}
	}
	return gid2shardIDs
}

func (kv *ShardKV) GetShardsData(request *ShardOperationRequest, response *ShardOperationResponse) {
	// only pull shards from leader
	if _, isLeader := kv.rf.GetState(); !isLeader {
		response.Err = ErrWrongLeader
		return
	}
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	defer DPrintf("{Node %v}{Group %v} processes PullTaskRequest %v with response %v", kv.rf.Me(), kv.gid, request, response)

	if kv.currentConfig.Num < request.ConfigNum {
		response.Err = ErrNotReady
		return
	}

	response.Shards = make(map[int]map[string]string)
	for _, shardID := range request.ShardIDs {
		response.Shards[shardID] = kv.stateMachines[shardID].deepCopy()
	}

	response.LastOperations = make(map[int64]OperationContext)
	for clientID, operation := range kv.lastOperations {
		response.LastOperations[clientID] = operation.deepCopy()
	}

	response.ConfigNum, response.Err = request.ConfigNum, OK
}

func (kv *ShardKV) applyInsertDataToShards(shardsInfo *ShardOperationResponse) *CommandResponse {
	if shardsInfo.ConfigNum == kv.currentConfig.Num {
		DPrintf("{Node %v}{Group %v} accepts shards insertion %v when currentConfig is %v", kv.rf.Me(), kv.gid, shardsInfo, kv.currentConfig)
		for shardId, shardData := range shardsInfo.Shards {
			shard := kv.stateMachines[shardId]
			if shard.Status == Pulling {
				for key, value := range shardData {
					shard.KV[key] = value
				}
				// 数据拉取完成，将本shard的状态更新成GCing，说明旧group下的该shard可以清除数据
				shard.Status = GCing
			} else {
				DPrintf("{Node %v}{Group %v} encounters duplicated shards insertion %v when currentConfig is %v", kv.rf.Me(), kv.gid, shardsInfo, kv.currentConfig)
				break
			}
		}

		// 在这里重新解释下LastOperationsmap用于客户端请求的幂等性判断
		// 调用GerShardsData同时需要拉取LastOperations数据的原因是防止拉取完成前后刚好客户端发送多次请求
		// 逻辑：如果新group未纪录过该clientId则直接插入该clientid 或 新group记录的clientid的cammandId小于拉取的对应的clientid的cammandid，则更新大的cammandid
		for clientId, operationContext := range shardsInfo.LastOperations {
			if lastOperation, ok := kv.lastOperations[clientId]; !ok || lastOperation.MaxAppliedCommandId < operationContext.MaxAppliedCommandId {
				kv.lastOperations[clientId] = operationContext
			}
		}
		return &CommandResponse{OK, ""}
	}
	DPrintf("{Node %v}{Group %v} rejects outdated shards insertion %v when currentConfig is %v", kv.rf.Me(), kv.gid, shardsInfo, kv.currentConfig)
	return &CommandResponse{ErrOutDated, ""}
}

// 三阶段
// 基本逻辑与二阶段类似，获取所有gcing的map，key依旧为旧groupid
func (kv *ShardKV) gcAction() {
	kv.mu.RLock()
	gid2shardIDs := kv.getShardIDsByStatus(GCing)
	var wg sync.WaitGroup
	for gid, shardIDs := range gid2shardIDs {
		DPrintf("{Node %v}{Group %v} starts a GCTask to delete shards %v in group %v when config is %v", kv.rf.Me(), kv.gid, shardIDs, gid, kv.currentConfig)
		wg.Add(1)
		go func(servers []string, configNum int, shardIDs []int) {
			defer wg.Done()
			gcTaskRequest := ShardOperationRequest{configNum, shardIDs}
			for _, server := range servers {
				var gcTaskResponse ShardOperationResponse
				srv := kv.makeEnd(server)
				if srv.Call("ShardKV.DeleteShardsData", &gcTaskRequest, &gcTaskResponse) && gcTaskResponse.Err == OK {
					DPrintf("{Node %v}{Group %v} deletes shards %v in remote group successfully when currentConfigNum is %v", kv.rf.Me(), kv.gid, shardIDs, configNum)
					// 远处旧group下的shards已经删除完成，将本group下的对应shard的状态从GCing置为serving
					kv.Execute(NewDeleteShardsCommand(&gcTaskRequest), &CommandResponse{})
				}
			}
		}(kv.lastConfig.Groups[gid], kv.currentConfig.Num, shardIDs)
	}
	kv.mu.RUnlock()
	wg.Wait()
}

func (kv *ShardKV) DeleteShardsData(request *ShardOperationRequest, response *ShardOperationResponse) {
	// only delete shards when role is leader
	if _, isLeader := kv.rf.GetState(); !isLeader {
		response.Err = ErrWrongLeader
		return
	}

	defer DPrintf("{Node %v}{Group %v} processes GCTaskRequest %v with response %v", kv.rf.Me(), kv.gid, request, response)

	kv.mu.RLock()
	if kv.currentConfig.Num > request.ConfigNum {
		DPrintf("{Node %v}{Group %v}'s encounters duplicated shards deletion %v when currentConfig is %v", kv.rf.Me(), kv.gid, request, kv.currentConfig)
		response.Err = OK
		kv.mu.RUnlock()
		return
	}
	kv.mu.RUnlock()
	var commandResponse CommandResponse
	// 只要涉及到数据修改的，都需要经过raft日志记录，所以需要调用execute，而不是步骤2的简单的数据复制，所以不需要经过raft，直接复制返回即可
	kv.Execute(NewDeleteShardsCommand(request), &commandResponse)
	response.Err = commandResponse.Err
}

func (kv *ShardKV) applyDeleteShards(shardsInfo *ShardOperationRequest) *CommandResponse {
	if shardsInfo.ConfigNum == kv.currentConfig.Num {
		DPrintf("{Node %v}{Group %v}'s shards status are %v before accepting shards deletion %v when currentConfig is %v", kv.rf.Me(), kv.gid, kv.getShardStatus(), shardsInfo, kv.currentConfig)
		for _, shardId := range shardsInfo.ShardIDs {
			shard := kv.stateMachines[shardId]
			if shard.Status == GCing {
				shard.Status = Serving
			} else if shard.Status == BePulling {
				kv.stateMachines[shardId] = NewShard()
			} else {
				DPrintf("{Node %v}{Group %v} encounters duplicated shards deletion %v when currentConfig is %v", kv.rf.Me(), kv.gid, shardsInfo, kv.currentConfig)
				break
			}
		}
		DPrintf("{Node %v}{Group %v}'s shards status are %v after accepting shards deletion %v when currentConfig is %v", kv.rf.Me(), kv.gid, kv.getShardStatus(), shardsInfo, kv.currentConfig)
		return &CommandResponse{OK, ""}
	}
	DPrintf("{Node %v}{Group %v}'s encounters duplicated shards deletion %v when currentConfig is %v", kv.rf.Me(), kv.gid, shardsInfo, kv.currentConfig)
	return &CommandResponse{OK, ""}
}

// no-op
func (kv *ShardKV) checkEntryInCurrentTermAction() {
	// HasLogInCurrentTerm 判断当前raft节点的最后一个日志是否等于当前的任期
	// 因为如果不等于，证明当前的leader节点(即自己)还未发送过任何日志，所以发送一个空日志进行日志同步
	if !kv.rf.HasLogInCurrentTerm() {
		// 关于内如raft集群同步日志的操作，无需rpc
		kv.Execute(NewEmptyEntryCommand(), &CommandResponse{})
	}
}

func (kv *ShardKV) applyEmptyEntry() *CommandResponse {
	return &CommandResponse{OK, ""}
}

