package dskv

import (
	"bytes"
	"time"

	"proxy/metric"
	"util/log"
	"model/pkg/kvrpcpb"
	"golang.org/x/net/context"
)

func (p *KvProxy) SqlInsert(req *kvrpcpb.InsertRequest, scope *kvrpcpb.Scope) ([]*kvrpcpb.InsertResponse, error) {
	var key, start, limit []byte
	var resp *kvrpcpb.InsertResponse
	var resps []*kvrpcpb.InsertResponse
	var route *KeyLocation
	var err error

	start = scope.Start
	limit = scope.Limit
	for {
		if key == nil {
			key = start
		} else if route != nil {
			key = route.EndKey
			// check key in range
			if bytes.Compare(key, start) < 0 || bytes.Compare(key, limit) >= 0 {
				// 遍历完成，直接退出循环
				break
			}
		} else {
			// must bug
			log.Error("invalid route, must bug!!!!!!!")
			return nil, ErrInternalError
		}
		resp, route, err = p.Insert(req, key)
		if err != nil {
			return nil, err
		}
		resps = append(resps, resp)
	}
	if len(resps) == 0 {
		log.Warn("SqlInsert: should not enter into here")
		resp = &kvrpcpb.InsertResponse{Code: 0, AffectedKeys: 0}
		resps = append(resps, resp)
	}
	return resps, nil
}

func (p *KvProxy) Insert(req *kvrpcpb.InsertRequest, key []byte) (*kvrpcpb.InsertResponse, *KeyLocation, error) {
	startTime := time.Now()
	in := GetRequest()
	defer PutRequest(in)
	in.Type = Type_Insert
	in.InsertReq = &kvrpcpb.DsInsertRequest{
		Header: &kvrpcpb.RequestHeader{},
		Req:    req,
	}

	bo := NewBackoffer(SetMaxBackoff, context.Background())
	resp, l, err := p.do(bo, in, key)
	delay := time.Now().Sub(startTime)
	if err != nil {
		metric.GsMetric.StoreApiMetric("KvInsert", false, delay)
	} else {
		metric.GsMetric.StoreApiMetric("KvInsert", true, delay)
	}
	if err != nil {
		return nil, nil, err
	}
	response := resp.GetInsertResp().GetResp()
	if response != nil && response.GetCode() == 0  && response.GetAffectedKeys() != uint64(len(req.Rows)) {
		var nodeId uint64 = 0
		l, err = p.RangeCache.LocateKey(bo, key)
		if l != nil {
			nodeId = l.NodeId
		}
		log.Error("nodeId:%d,requst:[%v],respose exception, respose:[%v] ",nodeId,req , response)
		return nil,nil,ErrAffectRows
	}
	if response == nil || response.GetCode() >0 {
		var nodeId uint64 = 0
		l, err = p.RangeCache.LocateKey(bo, key)
		if l != nil {
			nodeId = l.NodeId
		}
		log.Error("nodeId:%d,requst:[%v],respose exception, respose:[%v] ",nodeId,req , response)
		return response,nil,ErrInternalError
	}
	return response, l, nil
}

func (p *KvProxy) SqlQuery(req *kvrpcpb.SelectRequest, key []byte) (*kvrpcpb.SelectResponse, *KeyLocation, error) {
	startTime := time.Now()
	in := GetRequest()
	defer PutRequest(in)
	in.Type = Type_Select
	in.SelectReq = &kvrpcpb.DsSelectRequest{
		Header: &kvrpcpb.RequestHeader{},
		Req:    req,
	}

	bo := NewBackoffer(GetMaxBackoff, context.Background())
	resp, l, err := p.do(bo, in, key)
	delay := time.Now().Sub(startTime)
	if err != nil {
		metric.GsMetric.StoreApiMetric("KvQuery", false, delay)
	} else {
		metric.GsMetric.StoreApiMetric("KvQuery", true, delay)
	}
	if err != nil {
		return nil, nil, err
	}
	return resp.GetSelectResp().GetResp(), l, nil
}

func (p *KvProxy) SqlDelete(req *kvrpcpb.DeleteRequest, scope *kvrpcpb.Scope) ([]*kvrpcpb.DeleteResponse, error) {
	var key, start, limit []byte
	var resp *kvrpcpb.DeleteResponse
	var resps []*kvrpcpb.DeleteResponse
	var route *KeyLocation
	var err error

	start = scope.Start
	limit = scope.Limit
	for {
		if key == nil {
			key = start
		} else if route != nil {
			key = route.EndKey
			// check key in range
			if bytes.Compare(key, start) < 0 || bytes.Compare(key, limit) >= 0 {
				// 遍历完成，直接退出循环
				break
			}
		} else {
			return nil, ErrInternalError
		}
		resp, route, err = p.Delete(req, key)
		if err != nil {
			return nil, err
		}
		resps = append(resps, resp)
	}
	if len(resps) == 0 {
		resp = &kvrpcpb.DeleteResponse{Code: 0, AffectedKeys: 0}
		resps = append(resps, resp)
	}
	return resps, nil
}

func (p *KvProxy) Delete(req *kvrpcpb.DeleteRequest, key []byte) (*kvrpcpb.DeleteResponse, *KeyLocation, error) {
	start := time.Now()
	in := GetRequest()
	defer PutRequest(in)
	in.Type = Type_Delete
	in.DeleteReq = &kvrpcpb.DsDeleteRequest{
		Header: &kvrpcpb.RequestHeader{},
		Req:    req,
	}
	bo := NewBackoffer(ScannerNextMaxBackoff, context.Background())
	resp, l, err := p.do(bo, in, key)
	delay := time.Now().Sub(start)
	if err != nil {
		metric.GsMetric.StoreApiMetric("KvDelete", false, delay)
	} else {
		metric.GsMetric.StoreApiMetric("KvDelete", true, delay)
	}
	if err != nil {
		return nil, nil, err
	}
	return resp.GetDeleteResp().GetResp(), l, nil
}

type KvParisSlice []*kvrpcpb.KeyValue

func (p KvParisSlice) Len() int {
	return len(p)
}

func (p KvParisSlice) Swap(i int, j int) {
	p[i], p[j] = p[j], p[i]
}

func (p KvParisSlice) Less(i int, j int) bool {
	return bytes.Compare(p[i].GetKey(), p[j].GetKey()) < 0
}