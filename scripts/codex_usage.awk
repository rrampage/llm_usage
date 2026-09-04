BEGIN {
    FS = OFS = "\t"
    print "MODEL", "INPUT", "OUTPUT", "CACHE_RD", "CACHE_WR", "REASON", "TOTAL", "EVENTS"
}

{
    model = $1
    input[model] += $2
    output[model] += $3
    cache_read[model] += $4
    cache_write[model] += $5
    reasoning[model] += $6
    total[model] += $7
    events[model]++
}

END {
    for (model in events)
        print model, input[model], output[model], cache_read[model],
              cache_write[model], reasoning[model], total[model], events[model]
}
