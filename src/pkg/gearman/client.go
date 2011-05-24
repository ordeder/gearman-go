// Copyright 2011 Xing Xing <mikespook@gmail.com> All rights reserved.
// Use of this source code is governed by a MIT
// license that can be found in the LICENSE file.

package gearman

import (
    "os"
    "net"
    "sync"
    //    "log"
    "strconv"
)

/* 
The client side api for gearman.

usage:
    client = NewClient()
    client.AddServer("127.0.0.1:4730")
    handle := client.Do("foobar", []byte("data here"), JOB_LOW | JOB_BG)

*/
type Client struct {
    mutex    sync.Mutex
    conn     net.Conn
    JobQueue chan *ClientJob
    incoming chan []byte
    UId      uint32
}

// Create a new client.
func NewClient() (client *Client) {
    client = &Client{JobQueue: make(chan *ClientJob, QUEUE_CAP),
        incoming: make(chan []byte, QUEUE_CAP),
        UId:      1}
    return
}

// Add a server.
// In this version, one client connect to one job server.
// Sample is better. Plz do the load balancing by your self.
func (client *Client) AddServer(addr string) (err os.Error) {
    conn, err := net.Dial(TCP, addr)
    if err != nil {
        return
    }
    client.conn = conn
    return
}

// Internal read
func (client *Client) read() (data []byte, err os.Error) {
    if len(client.incoming) > 0 {
        // incoming queue is not empty
        data = <-client.incoming
    } else {
        // empty queue, read data from socket
        for {
            buf := make([]byte, BUFFER_SIZE)
            var n int
            if n, err = client.conn.Read(buf); err != nil {
                if err == os.EOF && n == 0 {
                    break
                }
                return
            }
            data = append(data, buf[0:n]...)
            if n < BUFFER_SIZE {
                break
            }
        }
    }
    // split package
    start, end := 0, 4
    tl := len(data)
    for i := 0; i < tl; i++ {
        if string(data[start:end]) == RES_STR {
            l := int(byteToUint32([4]byte{data[start+8], data[start+9], data[start+10], data[start+11]}))
            total := l + 12
            if total == tl {
                return
            } else {
                client.incoming <- data[total:]
                data = data[:total]
                return
            }
        } else {
            start++
            end++
        }
    }
    err = os.NewError("Invalid data struct.")
    return
}

// Read a job from job server.
// This function will return the job, and add it to the job queue.
func (client *Client) ReadJob() (job *ClientJob, err os.Error) {
    var rel []byte
    if rel, err = client.read(); err != nil {
        return
    }
    if job, err = DecodeClientJob(rel); err != nil {
        return
    } else {
        switch job.DataType {
        case ERROR:
            _, err = getError(job.Data)
            return
        case WORK_DATA, WORK_WARNING, WORK_STATUS, WORK_COMPLETE, WORK_FAIL, WORK_EXCEPTION:
            client.JobQueue <- job
        }
    }
    return
}

// Do the function.
// funcname is a string with function name.
// data is encoding to byte array.
// flag set the job type, include running level: JOB_LOW, JOB_NORMAL, JOB_HIGH,
// and if it is background job: JOB_BG.
// JOB_LOW | JOB_BG means the job is running with low level in background.
func (client *Client) Do(funcname string, data []byte, flag byte) (handle string, err os.Error) {
    var datatype uint32
    if flag&JOB_LOW == JOB_LOW {
        if flag&JOB_BG == JOB_BG {
            datatype = SUBMIT_JOB_LOW_BG
        } else {
            datatype = SUBMIT_JOB_LOW
        }
    } else if flag&JOB_HIGH == JOB_HIGH {
        if flag&JOB_BG == JOB_BG {
            datatype = SUBMIT_JOB_HIGH_BG
        } else {
            datatype = SUBMIT_JOB_HIGH
        }
    } else if flag&JOB_BG == JOB_BG {
        datatype = SUBMIT_JOB_BG
    } else {
        datatype = SUBMIT_JOB
    }

    rel := make([]byte, 0, 1024*64)
    rel = append(rel, []byte(funcname)...)
    rel = append(rel, '\x00')
    client.mutex.Lock()
    uid := strconv.Itoa(int(client.UId))
    client.UId++
    rel = append(rel, []byte(uid)...)
    client.mutex.Unlock()
    rel = append(rel, '\x00')
    rel = append(rel, data...)
    if err = client.WriteJob(NewClientJob(REQ, datatype, rel)); err != nil {
        return
    }
    var job *ClientJob
    if job, err = client.readLastJob(JOB_CREATED); err != nil {
        return
    }
    handle = string(job.Data)
    go func() {
        if flag&JOB_BG != JOB_BG {
            for {
                if job, err = client.ReadJob(); err != nil {
                    return
                }
                switch job.DataType {
                case WORK_DATA, WORK_WARNING:
                case WORK_STATUS:
                case WORK_COMPLETE, WORK_FAIL, WORK_EXCEPTION:
                    return
                }
            }
        }
    }()
    return
}

// Internal read last job
func (client *Client) readLastJob(datatype uint32) (job *ClientJob, err os.Error) {
    for {
        if job, err = client.ReadJob(); err != nil {
            return
        }
        if job.DataType == datatype {
            break
        }
    }
    if job.DataType != datatype {
        err = os.NewError("No job got.")
    }
    return
}

// Get job status from job server.
// !!!Not fully tested.!!!
func (client *Client) Status(handle string) (known, running bool, numerator, denominator uint, err os.Error) {

    if err = client.WriteJob(NewClientJob(REQ, GET_STATUS, []byte(handle))); err != nil {
        return
    }
    var job *ClientJob
    if job, err = client.readLastJob(STATUS_RES); err != nil {
        return
    }
    data := splitByteArray(job.Data, '\x00')
    if len(data) != 5 {
        err = os.NewError("Data Error.")
        return
    }
    if handle != string(data[0]) {
        err = os.NewError("Invalid handle.")
        return
    }
    known = data[1][0] == '1'
    running = data[2][0] == '1'
    if numerator, err = strconv.Atoui(string(data[3][0])); err != nil {
        return
    }
    if denominator, err = strconv.Atoui(string(data[4][0])); err != nil {
        return
    }
    return
}

// Send a something out, get the samething back.
func (client *Client) Echo(data []byte) (echo []byte, err os.Error) {
    if err = client.WriteJob(NewClientJob(REQ, ECHO_REQ, data)); err != nil {
        return
    }
    var job *ClientJob
    if job, err = client.readLastJob(ECHO_RES); err != nil {
        return
    }
    echo = job.Data
    return
}

// Get the last job.
// the job means a network package. 
// Normally, it is the job executed result.
func (client *Client) LastJob() (job *ClientJob) {
    if l := len(client.JobQueue); l != 1 {
        if l == 0 {
            return
        }
        for i := 0; i < l-1; i++ {
            <-client.JobQueue
        }
    }
    return <-client.JobQueue
}

// Send the job to job server.
func (client *Client) WriteJob(job *ClientJob) (err os.Error) {
    return client.write(job.Encode())
}

// Internal write
func (client *Client) write(buf []byte) (err os.Error) {
    var n int
    for i := 0; i < len(buf); i += n {
        n, err = client.conn.Write(buf[i:])
        if err != nil {
            return
        }
    }
    return
}

// Close.
func (client *Client) Close() (err os.Error) {
    err = client.conn.Close()
    close(client.JobQueue)
    return
}
