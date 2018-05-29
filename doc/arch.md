
# Range分裂
原来的设计是MS检查是否需要分裂，然后生成任务，通过range的心跳响应带回，这个设计的前提是range分片比较大，即使达到分裂门限后10s再获取分裂调度任务影响不是很大，可以容忍。

现在range大小比较小，这种设计就不能满足需求，因此range的分裂是range自己决策的，当range的大小达到分裂门限，首先计算中间key，然后主动向MS申请range分裂，MS返回新分裂的rangeID，以及新range的leader归属。

Range将分裂命令写入日志，各个range的副本独立分裂，分裂命令包括：new range，leader。

range设置一个检查门限check_size，分裂门限split_size和最大门限max_size(通常check_size是split_size的一半大小，max_size是split_size+check_size)，range记录每次写入的数据量大小（忽略delete引起的统计不准确），当数据量达到检查门限，遍历一次range，获得真实的range大小，如果range大小超过max_size触发分裂，range按照split_size分裂成两个range。每次检查之后check_size清零，这样可以减少检查次数。

具体的做法如下：

Range对象维护check_size, split_size, max_size, size, real_size, put操作触发size变化，当size>= check_size时，触发分裂检查，如果不满足分裂条件，那么size赋值为0，real_size更新为range真实的大小。同时我们一般配置check_size为split_size的一半大小，这样我们可以保证尽量减少分裂检查的次数，同时又能尽量减少出现连续分裂的情况。



分裂命令通过日志复制给所有副本，各个副本自行应用日志。分裂完成后被分裂的range主动向MS上报分裂成功，MS更新拓扑（这种上报一般不会失败，可以通过重试达到上报成功，即使出现多次重试失败，新的range生成后也会向MS上报心跳，达到拓扑更新的目标，这个作为最后的补救措施）。

分裂本身是range自身触发的，没有重复分裂的风险。

对于分裂结束后补充新副本，如果分裂日志被同步到新的副本也不会有问题，因为副本在分裂的时候首先需要检查range的范围是否包含新range的范围，以防止重复分裂和异常分裂。

单个rang的分裂只是range逻辑层面范围的修改，不会搬移数据，因此分裂本身会非常快完成，如果防止分裂，我们使用range的epoch[conf_version, version]，每次range分裂conf_version都会加一（+1），每次leader变更，version会加一(+1)，如果客户端提交的请求中携带的range epoch 比DS上的epoch低，那么DS返回stale epoch错误，客户端从MS获取最新的range epoch，然后继续访问DS。

补充：range epoch如果不匹配，单一的读命令和写命令如果key属于分裂后原range的范围，仍然可以访问，对于扫描和delete操作（范围delete）则只能拒绝。不过因为分裂本身比较快，分片也比较小，从MS拉取新的range拓扑再次重试可以接收，这样DS的处理逻辑也比较简单[p2] 。

Range的各个副本分裂出新的range后，根据分裂告知的leader，与leader建立连接。如果副本访问的leader已经变更，那么会收到not leader的错误，副本需要向MS询问range的leader。

Range各个副本执行分裂指令的时候，需要自行创建新的range。

# Range迁移
生成range迁移任务
Range leader心跳上报领取任务
Leader负责创建新的副本[p3] 
新副本从leader拉取快照
Leader需要记录当前所有副本的状态，心跳上报的时候需要把处于load snap以及down的副本信息都上报
任务执行中途失败怎么处理

任务中途失败的结果主要是产生残留range分片，因为合法的range group各个成员是确定的，一个不属于这个range group的分片向leader心跳获取拉取数据会被拒绝（成员认证），因此这个非法的range分片需要向MS查询leader信息，这个时候MS就会检查这个range分片的合法性，一旦发现这个分片不属于range group，那么就回复销毁命令，这个残留的range就会在DS被销毁。

注意：range迁移，新的副本在启动的时候需要首先自检，删除自身范围内所有数据，这样做的目的是防止脏数据污染副本的数据[p4] 。也就是说由leader发起的新建range需要自检，由MS发起的range新建则不需要，DS启动的时候recover所有的range的时候不触发自检。

# Range 故障检测和恢复
某个副本一定心跳周期没有向leader回复心跳那么leader就会认为该分片可能down，这个信息会随着leader向MS的心跳上报给MS，MS负责决策副本是否故障（可以直接认定或者想DS查询求证），生成故障恢复任务，通过心跳响应带回。

过程基本和range迁移一致，有一点需要说明的是，range需要先摘除故障的副本，然后添加新的副本（这个可以拆分成两个任务，一个是删除成员任务，一个是range在MS自检时触发的副本补齐任务）。

这里仍然是需要leader与副本之间的心跳作为前提保证的。

如果一个节点自身的N个心跳周期都没有想MS上报心跳，那么MS会认为这个节点发生故障，所有的副本都会被迁移到其他节点，同时这个节点不再接受新的副本创建。

# Range心跳
range的副本没必要都向MS上报心跳
range的leader上报心跳主要的目的是领取任务以及汇报当前各个副本的状态，MS主动推送任务这个架构不友好，之前的实践已经证明这一点（理由有两个：各个副本都上报，会造成信息冗余，MS压力增加，信息混乱等问题，由DS代理上报心跳会出现心跳包太大，延时增加以及传输超时等问题，另一个问题就是任务执行太集中，也不利于DS的稳定；MS主动推送任务，因为MS是有状态的，任务执行需要耗时，等待任务完成不利于MS的稳定，另外就是如果采用异步等待的方式也会造成MS需要维护task状态机，逻辑过于复杂；MS主动推送任务还有一个问题就是调度并发不好控制，离散度不好，过于集中的调度对DS压力太大，另外就是MS管理的range元数据信息已经很大，这种控制本身变得很复杂，没必要继续增加MS的复杂度[p5] ）
根据以上两点，task的具体执行就下推到range的leader执行，这样leader就需要关心各个副本的状态，因此leader与各个副本之间保持心跳就必得很有必要。
另外因为leader是MS指派的，就会出现一种场景，leader发生变更的时候，原来的leader没有收到leader变更通知（网络分裂等原因），虽然从客户端来的写落到pre leader不会写成功，但是读因为是本地读取，没有这个问题，会造成读取到脏数据，因此必须给leader一个租约，过期之前从MS哪里续租，如何续租呢，通过range的心跳续租比较合适，这样一旦pre leader的租约过期，并且不能从MS续租，那么就自动退化成follower，同时向MS询问最新的leader信息（以及成员信息，这些都可能已经发生变化，或者MS会触发failOver，删除这个副本）。
基于以上理由，leader与副本之间应该维护心跳，以保证整个系统的可靠稳定。

Leader与其他副本之间的心跳按照leader主动向其他副本发送心跳，其他副本响应心跳的模式。心跳信息中包含当前leader的版本（可能还可以包含其他必要的信息，这个后面根据情况添加）。

DS那么需要实现定时触发任务的组件。

DS定时扫描range，将range心跳加入心跳处理任务队列，DS采用grpc stream方式上报MS。

# Range成员管理
Range需要记录range group所有的成员，leader节点需要对所有的副本心跳以及日志请求进行认证，如果不属于range group，则需要拒绝服务，这样会触发非法副本向MS查询leader信息，MS会借机检查range的合法性，一旦非法，那么会销毁这个副本，达到清理残留的目的。

Range的成员变更必须通过leader来完成，leader以日志的形式通知其他成员range group的成员变更。

补充：range的副本一旦与leader失去联系就需要主动向MS查询leader信息，在此期间停止服务。

# Range拓扑变更以及client拓扑更新
原来我们的拓扑变更是通过长连接的方式进行，那时分片比较少，这种方案没什么问题，现在因为我们的分片很小，导致拓扑会很大，长连接推送的方式不可取了，粗略计算了一下，1PB的数据我们的元数据大小大约是8GB，这个量级使用长连接没办法实现，另外即使采用增量发送的方式也不可取，因为分片很小，拓扑变更比较频繁，如果想及时更新拓扑，那么这种增量计算也对MS的压力太大。因此需要采用拉取的模式，即以key去查询需要的分片，如果分片的epoch低被DS拒绝，再次从MS更新本分片的拓扑，因为我们的分裂是range分裂出去一部分，而不是分裂成两个完全新的range，因此原来的range永远都存活，这样既解决了拓扑更新的问题，又解决了MS压力大的问题。因为原数据在1PB数据规模的时候大约需要8GB空间，GS可以接受这个内存消耗。

扫描如果获取拓扑？

扫描操作本质上是对各个分片的扫描，按照GS现在的实现，可以以range的start key和end 可以作为key来更新拓扑，这部分逻辑现有的GS可以支持。

另外GS的拓扑树的生成方式需要改变，原来是每次生成一棵全量的树，现在需要对树进行修改完成拓扑修改。

原来MS把所有的元数据存储到磁盘上，同时MS的leader在内存中映射了一份，发生leader切换的时候，MS从磁盘上恢复数据即可，现在元数据很大，这种方式会在leader切换的时候耗费大量的时间，造成MS不可用，因此需要MS的三个副本在内存中实时维护元数据[p6] 。

Client缓存range的路由信息，缓存超时后需要向MS重新请求路由，这个过程是由client的读写指令触发的。

因为分片相对集群的容量来说很小，这样client会频繁的访问MS获取路由信息，我们可以通过一次查询返回连续的几个range的路由的方式减少请求次数（这个方案在GFS的实践中被证明很有效）。

# Range垃圾回收
MS创建range会出现创建失败，大多数情况是因为网络超时等原因导致MS不确定range是否创建成功。如果新创建的range是要加入到原有的range group中，那么因为range group中并没有添加这个成员副本，因此range group leader会拒绝这个副本的请求，DS定时扫描它管辖的range副本，发现这样的副本，就会在心跳上报的时候告知MS，MS确认可以删除此副本，就会下发删除副本的任务，由DS负责直接删除。

另外一种场景是，删除table，table管辖的所有的range都需要删除，MS采用惰性删除的方式删除这些range，MS首先将这个需要删除的table标记为删除，同时记录删除时间戳，这个操作需要持久化到MS的各个副本中，MS定期扫描这些标记为删除的table，默认MS会保留3天，三天后，正式删除这个table，range心跳上报时首先会根据元数据中的tableID查询table，如果不存在，则会查询标记为delete的table表，如果仍然查询失败，则触发删除range的任务，同时MS删除该range的元数据。

NOTE：table删除之后至少等待一个缓存TTL时间才能建立同名的table。

# DS 启动时恢复range
Range信息DS本地存储到rocksDB中（特殊前缀），一下场景需要更新DS中range到rocksDB。

Range创建
Range分裂
Range删除
Range成员变更
Range Leader变更
DS启动的时候，首先从rocksDB中读取range元数据，恢复range，然后通过range心跳上报MS。

MS 内存cache部分……



# DS注册下线
DS启动的时候

# MS range管理
## 创建range
两种场景下会触发MS创建range

Table创建。Table创建时，需要创建一个默认的range，只有一个副本，这样设计的目的是减少创建range失败的概率（如果一次性创建三个副本，就需要处理部分创建成功，部分创建失败的问题，不利于MS的设计和range垃圾回收）。MS通过range后续的心跳上报，检查副本情况，添加缺失的副本，这个过程不影响range的读写，同时简化的range创建失败时MS的处理流程。
Range添加成员。Range添加成员时，MS会事先在目标DS上创建这个range的副本。如果创建失败，则通过垃圾回收机制销毁这个失败的副本。
## Range成员变更
MS负载均衡策略会触发range添加成员，同时MS会检查range的副本数量是否符合配置（默认是三个副本），如果缺失副本，MS负责补充新的副本，如果副本数量超过配置，那么会删除副本。

NOTE：暂时不考虑缺失超过半数的副本的处理。

## Range故障恢复
如果有节点故障或者分片故障（磁盘文件checksum错误），那么MS只需要删除故障的节点即可。这样删除成功后，range缺少一个副本，MS会检测到，挑选一个range少的节点补充副本即可。

两个副本同时故障场景下的，存活的孤立副本不能选举出leader，孤立副本所在DS定期扫描，可以发现这个异常（需要配置一个窗口期，大于故障检测门限），DS心跳上报MS携带了这些异常信息，MS根据range的epoch信息判断是否需要修复故障副本，即通知DS上的这个range删除其他不可用的副本，完成leader选举，然后再 通过添加成员的方式补齐副本（MS需要记录range的最近一次心跳时间）。

NOTE：range读写store失败，则认为该副本故障，DS主动删除这个range副本，进而触发MS校验range的副本数量是否满足配置要求。

## Range删除
Range分裂的range创建由DS自行完成。



## 负载均衡
目前先支持两种负载均衡策略：

DS上range的数量均衡
主要目的是均衡磁盘占用率，这里假设各个DS的配置一致，后续再考虑不同DS配置下（本质就是允许的最大range数量不同）这个策略的优化。

DS心跳上报会上报目前DS上的range个数，leader个数，发送快照的range个数，接收和应用快照的range个数，以及split的range个数。目前的策略是将DS分成大于130%的平均range个数的组A和小于70%的平均range的组B，随机从组A中选择一个range，在组B选择一个DS，添加这个range的一个副本，这样这个range的副本数超过标准配置，然后MS会选择副本所在的DS中range个数多的那个副本，并把它从range group中删除，达到range数量均衡的目的。

DS上range的leader数量均衡
Leader的均衡策略跟range的均衡策略基本一致，区别在于leader的均衡只会发生在已有的副本上，range的数量均衡和leader的数量均衡配合使用才能基本达成集群的负载均衡。MS定期扫描调度，生成leader transfer任务，当range心跳上报后，触发MS主动向目标副本发送leader transfer任务，无论成功失败，均删除任务。Range 心跳响应不返回该任务。

负载均衡调度需要控制集群的调度的频率以及单个DS上的调度频率。集群的调度频率通过调度周期以及调度周期内的调度次数控制，单个DS上的调度频率就是依靠DS的心跳上报的发送快照的range个数，接收和应用快照的range个数，以及split的range个数，如果这些数量的占据DS上所有range数量的比例超过设定的门限时，这个节点暂时不参与调度。

NOTE：



# 集群监控
监控数据主要包括三部分：物理机维度的监控，进程维度的监控，分片维度的监控。

## 监控数据采集方式
采集方式有主动上报和查询两种，对于物理机和进程，查询和主动上报均可。但是对于分片的监控信息采集，查询的方式不是很好，因为分片会很多，另外分片的监控信息也不方便展示，更多的是作为热分片的调度的因素，因此分片的监控信息采集采用主动上报的方式，放在心跳请求包中。

在后续的分析中可以看到我们需要有agent，有进程，但是agent又不是必须的，只是作为容器化部署的组件而存在，因此我们的监控采用上报的方式来适配这种不确定性，我们只需要提供一个上报的地址和消息格式即可，这样我们不需要关心这些细节，以简化我们的设计。

## 物理机监控
物理机监控主要包括物理机的CPU，内存，磁盘，网络整体使用情况。

因为我们的DS进程应该会采用docker部署，这样的话，这种监控更多是反映宏观层面的数据，并不能直接推断出DS进程的资源使用情况。另外因为存在多个DS进程，因此DS进程本身采集这些宏观数据会引起数据的冗余和混乱，还有运行在容器中的实例很难获得宿主机的资源使用情况，基于这样的现实，需要一个agent程序专门负责宿主机的资源监控，同时我们考虑到后面DS的重启，升级等过程，agent应该是必不可少的。

## 进程监控
容器中运行的实例自身采集自身进程的资源情况应该跟宿主机上看到的进程的资源使用情况不太一样，这个结论有待确认。其实进程占用的CPU，内存，磁盘（进程独享一块独立的盘）这些数据也可以统一由agent采集，不过agent需要把进程ID和进程在master server那边分配的node ID绑定（进程ID因为重启会变化，其实应该是磁盘和nodeID绑定，这就需要）。这种绑定比较困难，因此最好进程自己能对自身使用的资源情况有统计。

另外进程监控中还包含对分片的监控，主要包括分片数量，角色是leader的分片数量，以及处于特殊状态的分片数量（分裂，拉取快照，发送快照），特殊状态的分片通常会产生较大的网络和磁盘流量，是master server进行调度的参与因素。

我们需要考虑进程直接运行在物理机上的情况，以及调试阶段的情况，因此进程也需要具有物理机信息采集和进程对系统资源使用情况的采集能力，这部分作为一个可以随机开启和关闭的组件存在。

## 分片监控
分片自身的监控主要是分片流量的监控，但是因为分片数量很多，每次都把分片的监控数据上报，会占用额外的网络带宽，我们只需要对分片的流量进行分段，划分为空闲，正常，忙三个状态即可，master server只需要根据这个状态信息做决策即可，不需要关心具体的流量数据，这样做的目的主要是出于节约心跳报文大小占用带宽和master server处理复杂度两个维度考虑。Debug阶段，可以通过配置定义这个划分的边界。



## 监控数据的保存
我们提供独立的监控数据处理模块metrics，它作为master server的一部分，也可以独立工作，metrics会被动接收集群上报的监控数据，这些监控数据会被定时写入存储，这个存储可以是sharkstore自身，也可以其他的存储。

同时metrics提供查询接口，以及watch接口，对订阅的服务持续提供监控数据的输出。Master server的调度组件也可以通过接口获得指定进程的资源占用情况，用于调度计算。



# master server 主要功能介绍
1、元数据的管理，创建DB,TABLE,预分裂等，获取range拓扑信息

2、心跳处理，负责data node, range的心跳处理，range的心跳会根据LEADER报上来的peers状态和数量，进行摘除，添加等

3、定时任务处理，处理耗时的一些任务，比如删表、均衡data node上的range的数量和range leader的数量、对故障节点上的range进行删除

4、负责range的分裂

5、监控数据



master server和data server交互主要有两种方式，对非实时要求的，通过上报上来的心跳，把任务从master server带下去处理，要求实时的直接调接口进行交互