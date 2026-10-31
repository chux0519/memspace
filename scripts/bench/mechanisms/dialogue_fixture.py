#!/usr/bin/env python3
"""Generate authored mechanism probes, not a held-out performance benchmark."""
import json
from pathlib import Path

# Concrete conditional/correction cases across operational and coding domains.
SCENES = [
('supermarket','Only change the supermarket staging connection pool. Production must remain 4.', 'Set it to 8.', 'Correction: staging should be 6. Leave production unchanged.', 'staging connection pool', '6', 'supermarket staging connection pool 的初次调整值和适用环境是什么？', 'What is the corrected supermarket staging pool size?', 'supermarket 哪个环境必须保持 4？', 'Who approved the supermarket staging pool change?'),
('checkout','结账服务只改沙箱的重试次数，正式环境仍是 2 次。', '已经设为 5 次。', '补充更正：沙箱改为 3 次，正式环境不动。', '沙箱', '3 次', 'What was the first checkout retry change and which environment did it apply to?', '结账沙箱最后改成几次重试？', 'Which checkout environment must retain two retries?', '结账沙箱重试变更是谁批准的？'),
('auth','For auth refresh tokens, change the test TTL only; production TTL stays 900 seconds.', 'Updated to 120 seconds.', 'Correction: use 180 seconds in test, production stays unchanged.', 'test', '180 seconds', '认证 refresh token 初次改了什么值、哪个环境？', 'What is the corrected test refresh token TTL?', '认证 token 哪个环境维持 900 秒？', 'Which security ticket approved the test TTL change?'),
('billing','账单任务仅在预发布调批大小，线上保持 20。', '已改成 50。', '更正：预发布批大小应为 35，线上照旧。', '预发布', '35', 'What was the initial billing batch size change and its scope?', '账单预发布批大小最终是多少？', 'Which billing environment keeps batch size 20?', '账单批大小调整的审批单号是什么？'),
('search','Search canary index only: alter shard count. The main index stays at 12.', 'Set to 3 shards.', 'Correction: canary needs 4 shards; main index is unchanged.', 'canary', '4 shards', '搜索分片第一次调整的值和适用索引是什么？', 'What shard count did the canary correction require?', '搜索哪个索引保持 12 个分片？', 'Which incident required the canary shard change?'),
('cache','缓存只调整开发环境过期时间，线上维持 600 秒。', '已调为 30 秒。', '修正：开发环境用 45 秒，线上不改。', '开发环境', '45 秒', 'What cache expiry value was initially set and where?', '缓存开发环境最终过期时间是多少？', 'Which cache environment keeps 600 seconds?', '缓存过期时间变更的负责人是谁？'),
('mobile','For mobile uploads change WiFi test chunk size only; cellular remains 256 KB.', 'Set to 1024 KB.', 'Correction: WiFi test should use 768 KB. Cellular stays unchanged.', 'WiFi test', '768 KB', '移动上传初次修改的分块大小和网络范围是什么？', 'What is the corrected WiFi test upload chunk size?', '移动上传哪种网络保持 256 KB？', 'Which device model was used to approve the upload change?'),
('analytics','分析管道只改欧洲测试分区的并发，美国生产分区保持 10。', '已经设成 16。', '更正：欧洲测试分区并发设 14，美国生产不变。', '欧洲测试分区', '14', 'What was the first analytics concurrency setting and target partition?', '分析欧洲测试分区最终并发是多少？', 'Which analytics partition keeps concurrency 10?', '分析并发变更的审核人是谁？'),
('queue','Only the email sandbox queue gets a new visibility timeout; live queue stays at 60 seconds.', 'Changed to 15 seconds.', 'Correction: use 25 seconds for sandbox, leave live alone.', 'sandbox', '25 seconds', '邮件队列初次超时调整值和适用队列是什么？', 'What is the corrected sandbox queue visibility timeout?', '邮件哪个队列保持 60 秒？', 'Which approval date was recorded for the email queue change?'),
('compiler','编译器只给实验目标更改优化等级，稳定目标仍是 O2。', '已改成 O0。', '更正：实验目标用 O1，稳定目标不动。', '实验目标', 'O1', 'What optimization level was first applied and to which compiler target?', '编译器实验目标最终优化等级是什么？', 'Which compiler target keeps O2?', '实验目标优化等级的审批链接是什么？'),
('storage','Storage replica change applies only to the lab bucket. Customer bucket remains at 3 replicas.', 'Changed to 1 replica.', 'Correction: lab needs 2 replicas; customer bucket stays unchanged.', 'lab', '2 replicas', '存储副本初次改了什么值、哪个桶？', 'What is the corrected lab bucket replica count?', '存储哪个桶保持 3 副本？', 'What customer signed off on the lab replica change?'),
('scheduler','调度器只改离线测试任务的间隔，在线任务保持 60 分钟。', '已调为 10 分钟。', '修正：离线测试间隔设为 15 分钟，在线任务不变。', '离线测试', '15 分钟', 'What was the first scheduler interval change and which job class?', '调度器离线测试任务最终间隔是多少？', 'Which scheduler jobs keep a 60 minute interval?', '调度器间隔变更的审批编号是什么？'),
]
def generate():
    c={'version':'dialogue-mechanisms-v1','raw_turns':True,'projects':['dialogue-probes'],'documents':[],'questions':[]}
    for tag,start,reply,fix,scope,value,q1,q2,q3,q4 in SCENES:
        ids=[tag+'-'+s for s in ('condition','reply','correction','ack')]
        for i,(body,ev) in enumerate([(start,scope),(reply,reply),(fix,value),('Acknowledged.','Acknowledged.')]):
            c['documents'].append(dict(id=ids[i],project='dialogue-probes',session=tag,text=body,evidence=ev,event_type='user.prompt' if i%2==0 else 'agent.turn_completed'))
        for i,(q,g) in enumerate([(q1,[[ids[0]],[ids[1]]]),(q2,[[ids[2]]]),(q3,[[ids[0]]]),(q4,[])]):
            c['questions'].append(dict(id=f'{tag}-q{i+1}',split='test',category=['condition_and_value','correction','protected_scope','missing_approval'][i],query=q,project='dialogue-probes',scope='current_project',evidence_groups=g,budget=6000))
    return c
if __name__=='__main__':
    target=Path(__file__).resolve().parents[1]/'data'/'dialogue-mechanisms-v1.json'
    target.write_text(json.dumps(generate(),ensure_ascii=False,indent=2)+'\n')
