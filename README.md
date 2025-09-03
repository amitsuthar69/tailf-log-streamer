### Start server :

```
go run main.go
```

---

### Get Deps:

```
go get github.com/gorilla/websocket
go get github.com/fsnotify/fsnotify
```

---

### Log Simulator

```go
package main

import (
	"fmt"
	"log"
	"os"
	"time"
)

func main() {
	f, err := os.OpenFile("/path/to/temp.file",
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Println(err)
		return
	}
	defer f.Close()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	fmt.Println("Started appending logs...")

	for range ticker.C {
		if _, err := f.WriteString("03/22 08:53:00 INFO:......log......\n"); err != nil {
			log.Println(err)
		}
	}
}
```
