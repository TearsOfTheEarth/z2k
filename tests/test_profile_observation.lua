local H=dofile('tests/lib/detector_harness.lua')
local fork=os.getenv('Z2K_FORK_DIR') or '../zapret2-z2k-fork'
dofile(fork..'/lua/zapret-antidpi.lua')
dofile('files/lua/z2k-modern-core.lua')
VERDICT_DROP=1
instance_cutoff=function() end
local function profiles(path)
    local result={}
    for line in io.lines(path) do
        local range_in,range_out,payload='x','a','all'
        local instances,key={}
        for tok in line:gmatch('%S+') do
            if tok:match('^%-%-in%-range=') then range_in=tok:match('=(.*)')
            elseif tok:match('^%-%-out%-range=') then range_out=tok:match('=(.*)')
            elseif tok:match('^%-%-payload=') then payload=tok:match('=(.*)')
            elseif tok:match('^%-%-lua%-desync=') then
                local value=tok:match('=(.*)'); local fn=value:match('^[^:]+'); local arg={}
                for part in value:gmatch(':([^:]+)') do
                    local k,v=part:match('^([^=]+)=(.*)$')
                    arg[k or part]=v or true
                end
                local instance={func=fn,arg=arg,range_in=range_in,range_out=range_out,payload=payload}
                instances[#instances+1]=instance
                if fn=='circular' then key=arg.key end
            end
        end
        if key then result[key]=instances end
    end
    return result
end
local all=profiles(assert(os.getenv('Z2K_PROFILE_FIXTURE')))
local no_reset=profiles(assert(os.getenv('Z2K_PROFILE_NO_RESET')))
local function circular_instance(pool)
    for _,ins in ipairs(assert(all[pool],pool)) do if ins.func=='circular' then return ins end end
    error('missing circular: '..pool)
end
H.test('every shipped TCP pool uses retrans=2, quorum 3, and the reset opt-out',function()
    for _,key in ipairs({'rkn_tcp','yt_tcp','gv_tcp','http_rkn'}) do
        local c=circular_instance(key)
        H.eq('2',c.arg.retrans); H.eq('3',c.arg.fails); H.eq(true,c.arg.reset)
        H.eq('-s5556',c.range_in); H.eq('all',c.payload)
        for _,ins in ipairs(no_reset[key]) do if ins.func=='circular' then H.eq(nil,ins.arg.reset) end end
    end
    H.eq('60',circular_instance('rkn_tcp').arg.time)
    H.eq('300',circular_instance('yt_tcp').arg.time)
    H.eq('300',circular_instance('gv_tcp').arg.time)
end)
H.test('QUIC drops only outgoing Initials, never replies or later packets',function()
    local checked=0
    for _,ins in ipairs(all.yt_quic) do
        if ins.func=='drop' then
            checked=checked+1
            for _,out in ipairs({true,false}) do
                for _,kind in ipairs({'quic_initial','unknown'}) do
                    local d=H.udp(nil,out,'data',kind); d.arg=ins.arg; d.func_instance='drop_'..ins.arg.strategy
                    H.eq(out and kind=='quic_initial' and VERDICT_DROP or nil,drop(nil,d))
                end
            end
        end
    end
    assert(checked>0,'missing replacement drop fixture')
end)
H.test('Discord observer sees replies and unknown data while fakes stay within outbound d4',function()
    local c=circular_instance('discord_udp')
    H.eq('a',c.range_in); H.eq('a',c.range_out); H.eq('all',c.payload)
    local d=H.udp(nil,true,'discovery','discord_ip_discovery'); d.arg=c.arg
    local h=H.step(d); h.failure_counter=2
    local reply=H.udp(d.track,false,'reply','unknown',1,2); reply.arg=c.arg
    H.step(reply); H.eq(nil,h.failure_counter)
    for _,ins in ipairs(all.discord_udp) do
        if ins.func~='circular' then
            H.eq('x',ins.range_in); H.eq('-d4',ins.range_out)
            H.eq('discord_ip_discovery,stun',ins.payload)
        end
    end
end)
H.test('host scope separates public suffixes and unrelated API services',function()
    for _,key in ipairs({'rkn_tcp','yt_tcp','gv_tcp','yt_quic'}) do
        local arg=circular_instance(key).arg
        H.eq('z2k_service_hostkey',arg.hostkey)
        for _,host in ipairs({'youtube.co.uk','bbc.co.uk','fonts.googleapis.com','youtubei.googleapis.com'}) do
            local d=H.tcp(H.track(host),true,1,'x'); d.arg=arg
            H.eq(host..'|4',z2k_service_hostkey(d))
            H.eq(host,d.track.hostname); H.eq('0',arg.nld)
        end
    end
end)
H.test('only the explicit video CDN is grouped and families remain separate',function()
    for _,key in ipairs({'gv_tcp','yt_quic'}) do
        local arg=circular_instance(key).arg
        for _,host in ipairs({'rr1.googlevideo.com','rr2.googlevideo.com'}) do
            local d=H.tcp(H.track(host),true,1,'x'); d.arg=arg
            H.eq('googlevideo.com|4',z2k_service_hostkey(d))
            d.dis.ip=nil; d.dis.ip6={}; H.eq('googlevideo.com|6',z2k_service_hostkey(d))
        end
        local d=H.tcp(H.track('googlevideo.com.example.org'),true,1,'x'); d.arg=arg
        H.eq('googlevideo.com.example.org|4',z2k_service_hostkey(d))
    end
end)
H.test('a success on another API host cannot reset failures for this host',function()
    local arg=circular_instance('yt_tcp').arg
    local d=H.tcp(H.track('youtubei.googleapis.com'),true,1,H.client,'tls_client_hello'); d.arg=arg
    local h=H.step(d); h.failure_counter=2
    local other=H.tcp(H.track('fonts.googleapis.com'),false,1,H.hello,'tls_server_hello'); other.arg=arg; H.step(other)
    other=H.tcp(other.track,false,4200,'new content'); other.arg=arg; H.step(other)
    H.eq(2,h.failure_counter)
end)
H.test('previously unprotected TCP fakes carry an invalid checksum',function()
    for key,num in pairs({rkn_tcp='38',yt_tcp='18',gv_tcp='18'}) do
        local checked=false
        for _,ins in ipairs(all[key]) do
            if ins.func=='fake' and ins.arg.strategy==num then
                H.eq(true,ins.arg.badsum); checked=true
            end
        end
        assert(checked,'missing fake '..key)
    end
end)
H.test('GV slot 11 is distinct while the existing 1..22 numbering is retained',function()
    local ids,slot6,slot11={}
    for _,ins in ipairs(all.gv_tcp) do
        if ins.arg.strategy then ids[tonumber(ins.arg.strategy)]=true end
        if ins.func=='multisplit' and ins.arg.strategy=='6' then slot6=ins.arg end
        if ins.func=='multisplit' and ins.arg.strategy=='11' then slot11=ins.arg end
    end
    for i=1,22 do H.eq(true,ids[i]) end
    H.eq('1',slot6.seqovl); H.eq(nil,slot11.seqovl)
    H.eq(slot6.pos,slot11.pos)
end)
H.finish()
