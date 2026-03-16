TigerBettle powered BYOC credit system
========

Given a Control Plane and a set of _M_ compute pools ("shoots"):
* Control Plane exposes RPC apis (thinking ConnectRPC for this), which pushes messages onto Kafka (w/ protobuf encoded msgs)
* N workers consume from this Kafka, and
   * do the relational stuff to postgres
   * prepare batches for tigerbeetle
* simple dashboard served via Control Plane
* a simple mechanism to spin up M "shoot" clusters that communicate with the control plane 
   * at fixed intervals?
   * record respurce usage on the shoopt cluster to the control plane (any slicebale resources, cpu, memory, "exotic" resources like gpu slickes, disk usage)
   * this acts as heartbeat. if failed propogate a "kill" command in the shoot to stop stuff

