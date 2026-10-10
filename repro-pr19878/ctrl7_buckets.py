import json, os, urllib.request
ES=os.environ.get("REPRO_ES_HOST", "http://127.0.0.1:9200"); IDX="ctrl7_buckets"; K=70; P=65536//K
def req(m,p,b=None,ct="application/json"):
    r=urllib.request.Request(ES+p,data=b,method=m,headers={"Content-Type":ct})
    try: return json.load(urllib.request.urlopen(r))
    except urllib.error.HTTPError as e: return json.load(e)
req("DELETE","/"+IDX)
req("PUT","/"+IDX,json.dumps({"settings":{"number_of_shards":2,"number_of_replicas":0},"mappings":{"properties":{"meta_fields":{"type":"object","dynamic":True}}}}).encode())
lines=[]
for d in range(P):
    lines.append(json.dumps({"index":{"_index":IDX,"_id":str(d)}})); lines.append(json.dumps({"meta_fields":{f"k{k:02d}":f"v{d}" for k in range(K)}}))
req("POST","/_bulk?refresh=true",("\n".join(lines)+"\n").encode(),"application/x-ndjson")
comp={f"vs_k{k:02d}":{"composite":{"size":P,"sources":[{f"k{k:02d}":{"terms":{"field":f"meta_fields.k{k:02d}.keyword"}}}]}} for k in range(K)}
cov={"coverage":{"filters":{"filters":{f"k{k:02d}":{"term":{"_ignored":f"meta_fields.k{k:02d}.keyword"}} for k in range(K)}}}}
for name,aggs in [("composites only",comp),("composites + coverage filters",{**comp,**cov})]:
    r=req("POST",f"/{IDX}/_search",json.dumps({"size":0,"aggs":aggs}).encode())
    print(f"{name}: {len(aggs)} aggs ->", r.get("error",{}).get("caused_by",{}).get("type") or "OK")
req("DELETE","/"+IDX)
