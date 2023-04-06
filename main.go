package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"github.com/go-restruct/restruct"
	"github.com/google/uuid"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"pyEyenStaller/marshal"
	"strings"
	// "github.com/k0kubun/pp/v3"
)

type Output map[string][]byte

type PyInstArchive struct {
	fileBytes               []byte
	Output                  Output
	bytesPtr                io.ReadSeeker
	fileSize                int64
	cookiePosition          int64
	pyInstVersion           int64
	pythonMajorVersion      int
	pythonMinorVersion      int
	overlaySize             int64
	overlayPosition         int64
	tableOfContentsSize     int64
	tableOfContentsPosition int64
	tableOfContents         []CTOCEntry
	pycMagic                [4]byte
	gotPycMagic             bool
	barePycsList            []string
}

func (p *PyInstArchive) Open() bool {
	p.fileSize = int64(len(p.fileBytes))
	p.bytesPtr = bytes.NewReader(p.fileBytes)
	return true
}

func (p *PyInstArchive) CheckFile() bool {
	fmt.Printf("[+] Processing\n")

	var searchChunkSize int64 = 8192
	endPosition := p.fileSize
	p.cookiePosition = -1

	if endPosition < int64(len(PYINST_MAGIC)) {
		fmt.Println("[!] Error : File is too short or truncated")
		return false
	}

	var startPosition, chunkSize int64
	for {
		if endPosition >= searchChunkSize {
			startPosition = endPosition - searchChunkSize
		} else {
			startPosition = 0
		}
		chunkSize = endPosition - startPosition
		if chunkSize < int64(len(PYINST_MAGIC)) {
			break
		}

		if _, err := p.bytesPtr.Seek(startPosition, io.SeekStart); err != nil {
			fmt.Println("[!] File seek failed")
			return false
		}
		var data []byte = make([]byte, searchChunkSize)
		p.bytesPtr.Read(data)

		if offs := bytes.Index(data, PYINST_MAGIC[:]); offs != -1 {
			p.cookiePosition = startPosition + int64(offs)
			break
		}
		endPosition = startPosition + int64(len(PYINST_MAGIC)) - 1

		if startPosition == 0 {
			break
		}
	}
	if p.cookiePosition == -1 {
		fmt.Println("[!] Error : Missing cookie, unsupported pyinstaller version or not a pyinstaller archive")
		return false
	}
	p.bytesPtr.Seek(p.cookiePosition+PYINST20_COOKIE_SIZE, io.SeekStart)

	var cookie []byte = make([]byte, 64)
	if _, err := p.bytesPtr.Read(cookie); err != nil {
		fmt.Println("[!] Failed to read cookie!")
		return false
	}

	cookie = bytes.ToLower(cookie)
	if bytes.Contains(cookie, []byte("python")) {
		p.pyInstVersion = 21
		fmt.Println("[+] Pyinstaller version: 2.1+")
	} else {
		p.pyInstVersion = 20
		fmt.Println("[+] Pyinstaller version: 2.0")
	}
	return true
}

func (p *PyInstArchive) GetCArchiveInfo() bool {
	failFunc := func() bool {
		fmt.Println("[!] Error : The file is not a pyinstaller archive")
		return false
	}

	getPyMajMinVersion := func(version int) (int, int) {
		if version >= 100 {
			return version / 100, version % 100
		}
		return version / 10, version % 10
	}

	printPythonVerLenPkg := func(pyMajVer, pyMinVer int, lenPkg uint) {
		fmt.Printf("[+] Python version: %d.%d\n", pyMajVer, pyMinVer)
		fmt.Printf("[+] Length of package: %d bytes\n", lenPkg)
	}

	calculateTocPosition := func(cookieSize int, lengthOfPackage, toc uint, tocLen int) {
		// Additional data after the cookie
		tailBytes := p.fileSize - p.cookiePosition - int64(cookieSize)

		// Overlay is the data appended at the end of the PE
		p.overlaySize = int64(lengthOfPackage) + tailBytes
		p.overlayPosition = p.fileSize - p.overlaySize
		p.tableOfContentsPosition = p.overlayPosition + int64(toc)
		p.tableOfContentsSize = int64(tocLen)
	}

	if _, err := p.bytesPtr.Seek(p.cookiePosition, io.SeekStart); err != nil {
		return failFunc()
	}

	if p.pyInstVersion == 20 {
		var pyInst20Cookie PyInst20Cookie
		cookieBuf := make([]byte, PYINST20_COOKIE_SIZE)
		if _, err := p.bytesPtr.Read(cookieBuf); err != nil {
			return failFunc()
		}

		if err := restruct.Unpack(cookieBuf, binary.LittleEndian, &pyInst20Cookie); err != nil {
			return failFunc()
		}

		p.pythonMajorVersion, p.pythonMinorVersion = getPyMajMinVersion(pyInst20Cookie.PythonVersion)
		printPythonVerLenPkg(p.pythonMajorVersion, p.pythonMinorVersion, uint(pyInst20Cookie.LengthOfPackage))

		calculateTocPosition(
			PYINST20_COOKIE_SIZE,
			uint(pyInst20Cookie.LengthOfPackage),
			uint(pyInst20Cookie.Toc),
			pyInst20Cookie.TocLen,
		)

	} else {
		var pyInst21Cookie PyInst21Cookie
		cookieBuf := make([]byte, PYINST21_COOKIE_SIZE)
		if _, err := p.bytesPtr.Read(cookieBuf); err != nil {
			return failFunc()
		}
		if err := restruct.Unpack(cookieBuf, binary.LittleEndian, &pyInst21Cookie); err != nil {
			return failFunc()
		}
		fmt.Println("[+] Python library file:", string(bytes.Trim(pyInst21Cookie.PythonLibName, "\x00")))
		p.pythonMajorVersion, p.pythonMinorVersion = getPyMajMinVersion(pyInst21Cookie.PythonVersion)
		printPythonVerLenPkg(p.pythonMajorVersion, p.pythonMinorVersion, pyInst21Cookie.LengthOfPackage)

		calculateTocPosition(
			PYINST21_COOKIE_SIZE,
			pyInst21Cookie.LengthOfPackage,
			pyInst21Cookie.Toc,
			pyInst21Cookie.TocLen,
		)
	}
	return true
}

func (p *PyInstArchive) ParseTOC() {
	const CTOCEntryStructSize = 18
	p.bytesPtr.Seek(p.tableOfContentsPosition, io.SeekStart)

	var parsedLen int64 = 0

	// Parse table of contents
	for {
		if parsedLen >= p.tableOfContentsSize {
			break
		}
		var ctocEntry CTOCEntry

		data := make([]byte, CTOCEntryStructSize)
		p.bytesPtr.Read(data)
		restruct.Unpack(data, binary.LittleEndian, &ctocEntry)

		nameBuffer := make([]byte, ctocEntry.EntrySize-CTOCEntryStructSize)
		p.bytesPtr.Read(nameBuffer)

		nameBuffer = bytes.TrimRight(nameBuffer, "\x00")
		if len(nameBuffer) == 0 {
			ctocEntry.Name = randomString()
			fmt.Printf("[!] Warning: Found an unamed file in CArchive. Using random name %s\n", ctocEntry.Name)
		} else {
			ctocEntry.Name = string(nameBuffer)
		}

		// fmt.Printf("%+v\n", ctocEntry)
		p.tableOfContents = append(p.tableOfContents, ctocEntry)
		parsedLen += int64(ctocEntry.EntrySize)
	}
	fmt.Printf("[+] Found %d files in CArchive\n", len(p.tableOfContents))
}

func (p *PyInstArchive) ExtractFiles() {
	fmt.Println("[+] Beginning extraction...please standby")

	for _, entry := range p.tableOfContents {
		p.bytesPtr.Seek(p.overlayPosition+int64(entry.EntryPosition), io.SeekStart)
		data := make([]byte, entry.DataSize)
		p.bytesPtr.Read(data)

		if entry.ComressionFlag == 1 {
			var err error
			compressedData := data[:]
			data, err = zlibDecompress(compressedData)
			if err != nil {
				fmt.Printf("[!] Error: Failed to decompress %s in CArchive, extracting as-is", entry.Name)
				p.Output[entry.Name] = compressedData

				continue
			}

			if uint(len(data)) != entry.UncompressedDataSize {
				fmt.Printf("[!] Warning: Decompressed size mismatch for file %s\n", entry.Name)
			}
		}

		if entry.TypeCompressedData == 'd' || entry.TypeCompressedData == 'o' {
			// d -> ARCHIVE_ITEM_DEPENDENCY
			// o -> ARCHIVE_ITEM_RUNTIME_OPTION
			// These are runtime options, not files
			continue
		}

		if entry.TypeCompressedData == 's' {
			// s -> ARCHIVE_ITEM_PYSOURCE
			// Entry point are expected to be python scripts
			fmt.Printf("[+] Possible entry point: %s.pyc\n", entry.Name)
			if !p.gotPycMagic {
				// if we don't have the pyc header yet, fix them in a later pass
				fmt.Println("[+] Storing pyc header for later")
				p.barePycsList = append(p.barePycsList, entry.Name+".pyc")
			}
			p.writePyc(entry.Name+".pyc", data)
		} else if entry.TypeCompressedData == 'M' || entry.TypeCompressedData == 'm' {
			// M -> ARCHIVE_ITEM_PYPACKAGE
			// m -> ARCHIVE_ITEM_PYMODULE
			// packages and modules are pyc files with their header intact

			// From PyInstaller 5.3 and above pyc headers are no longer stored
			// https://github.com/pyinstaller/pyinstaller/commit/a97fdf
			if data[2] == '\r' && data[3] == '\n' {
				// < pyinstaller 5.3
				if !p.gotPycMagic {
					copy(p.pycMagic[:], data[0:4])
					p.gotPycMagic = true
				}

				p.writePyc(entry.Name+".pyc", data)
			} else {
				// >= pyinstaller 5.3
				if !p.gotPycMagic {
					// if we don't have the pyc header yet, fix them in a later pass
					p.barePycsList = append(p.barePycsList, entry.Name+".pyc")
				}
				p.Output[entry.Name+".pyc"] = data
			}
		} else {

			p.Output[entry.Name] = data
		}
	}

	for name, entry := range p.Output {
		if strings.HasSuffix(name, ".pyz") {
			if p.pythonMajorVersion == 3 {
				fmt.Println("[+] Extracting", name)
				p.extractPYZ(entry)
			} else {
				fmt.Printf("[!] Skipping pyz extraction as Python %d.%d is not supported\n", p.pythonMajorVersion, p.pythonMinorVersion)
			}
		}
	}

	p.fixBarePycs()
}

func (p *PyInstArchive) fixBarePycs() {
	for _, pycFile := range p.barePycsList {
		for name, entry := range p.Output {
			if name == pycFile {
				fmt.Printf("[+] Fixing header of file %s\n", pycFile)
				newBytes := entry
				copy(newBytes[0:4], p.pycMagic[:])
				p.Output[name] = newBytes
				break
			}
		}
	}
}

func (p *PyInstArchive) extractPYZ(data []byte) {
	f := bytes.NewReader(data)

	var pyzMagic []byte = make([]byte, 4)
	f.Read(pyzMagic)
	if !bytes.Equal(pyzMagic, []byte("PYZ\x00")) {
		fmt.Println("[!] Magic header in PYZ archive doesn't match")
	}

	var pyzPycMagic []byte = make([]byte, 4)
	f.Read(pyzPycMagic)

	if !p.gotPycMagic {
		copy(p.pycMagic[:], pyzPycMagic)
		p.gotPycMagic = true
	} else if !bytes.Equal(p.pycMagic[:], pyzPycMagic) {
		copy(p.pycMagic[:], pyzPycMagic)
		p.gotPycMagic = true
		fmt.Println("[!] Warning: pyc magic of files inside PYZ archive are different from those in CArchive")
	}

	var pyzTocPositionBytes []byte = make([]byte, 4)
	f.Read(pyzTocPositionBytes)
	pyzTocPosition := binary.BigEndian.Uint32(pyzTocPositionBytes)
	f.Seek(int64(pyzTocPosition), io.SeekStart)

	su := marshal.NewUnmarshaler(f)
	obj := su.Unmarshal()
	if obj == nil {
		fmt.Println("Unmarshalling failed")
	} else {
		// pp.Print(obj)
		listobj := obj.(*marshal.PyListObject)
		listobjItems := listobj.GetItems()
		fmt.Printf("[+] Found %d files in PYZArchive\n", len(listobjItems))

		for _, item := range listobjItems {
			item := item.(*marshal.PyListObject)
			name := item.GetItems()[0].(*marshal.PyStringObject).GetString()

			ispkg_position_length_tuple := item.GetItems()[1].(*marshal.PyListObject)
			ispkg := ispkg_position_length_tuple.GetItems()[0].(*marshal.PyIntegerObject).GetValue()
			position := ispkg_position_length_tuple.GetItems()[1].(*marshal.PyIntegerObject).GetValue()
			length := ispkg_position_length_tuple.GetItems()[2].(*marshal.PyIntegerObject).GetValue()

			// Prevent writing outside dirName
			filename := strings.ReplaceAll(name, "..", "__")
			filename = strings.ReplaceAll(filename, ".", string(os.PathSeparator))

			var filenamepath string
			if ispkg == 1 {
				filenamepath = filepath.Join(filename, "__init__.pyc")
			} else {
				filenamepath = filepath.Join(filename + ".pyc")
			}

			f.Seek(int64(position), io.SeekStart)

			var compressedData []byte = make([]byte, length)
			f.Read(compressedData)

			decompressedData, err := zlibDecompress(compressedData)
			if err != nil {
				fmt.Printf("[!] Error: Failed to decompress %s in PYZArchive, likely encrypted. Extracting as is", filenamepath)
				p.Output[filenamepath+".pyc.encrypted"] = compressedData
			} else {
				p.writePyc(filenamepath, decompressedData)
			}
		}
	}
}

func (p *PyInstArchive) writePyc(path string, data []byte) {
	f := bytes.NewBuffer(nil)
	f.Write(p.pycMagic[:])

	if p.pythonMajorVersion >= 3 && p.pythonMinorVersion >= 7 {
		// PEP 552 -- Deterministic pycs
		f.Write([]byte{0, 0, 0, 0})             //Bitfield
		f.Write([]byte{0, 0, 0, 0, 0, 0, 0, 0}) //(Timestamp + size) || hash
	} else {
		f.Write([]byte{0, 0, 0, 0}) //Timestamp
		if p.pythonMajorVersion >= 3 && p.pythonMinorVersion >= 3 {
			f.Write([]byte{0, 0, 0, 0})
		}
	}
	f.Write(data)
	p.Output[path] = f.Bytes()
}

func extract_exe(fileBytes []byte) PyInstArchive {
	arch := PyInstArchive{fileBytes: fileBytes, Output: make(map[string][]byte)}

	if arch.Open() {
		if arch.CheckFile() {
			if arch.GetCArchiveInfo() {
				arch.ParseTOC()
				arch.ExtractFiles()
				fmt.Printf("[+] Successfully extracted pyinstaller archive\n")
				fmt.Println("\nYou can now use a python decompiler on the pyc files within the extracted directory")
			}
		}
	}
	return arch
}

func (a PyInstArchive) decompile(writer io.ReadWriter) {

	newZip := zip.NewWriter(writer)

	for name, entry := range a.Output {
		newUuid := uuid.NewString()
		if path.Ext(name) != ".pyc" {
			continue
		}
		writeName := strings.ReplaceAll(name, "/", "_")
		fmt.Println("Writing file: " + writeName)
		f, err := os.Create(newUuid + "_" + writeName)
		defer f.Close()
		defer os.Remove(newUuid + "_" + writeName)
		if err != nil {
			fmt.Println("Error writing file")
			return
		}
		f.Write(entry)

		stdOutBuf := bytes.NewBuffer(nil)

		cmd := exec.Command("pycdc", newUuid+"_"+writeName)
		cmd.Stdout = stdOutBuf
		err = cmd.Run()
		if err != nil {
			fmt.Println(err.Error())
			continue
		}
		cmd.Wait()
		fWriter, err := newZip.Create(writeName + ".py")
		if err != nil {
			fmt.Println(err.Error())
		} else {
			fWriter.Write(stdOutBuf.Bytes())
		}
		//os.Remove(fName)
		if err != nil {
			fmt.Println(err.Error())
		}
	}
	newZip.Close()

}

func ProcessPyinstall(fileBytes []byte) (PyInstArchive, bytes.Buffer, error) {

	a := extract_exe(fileBytes)

	zipBuffer := bytes.NewBuffer(nil)

	a.decompile(zipBuffer)

	zipFile, err := os.Create("extracted.zip")
	if err != nil {
		fmt.Println(err.Error())
	} else {
		zipFile.Write(zipBuffer.Bytes())
	}
	return a, *zipBuffer, err
}

func main() {
	fileName := flag.String("file", "", "File to extract")
	apiMode := flag.Bool("api", false, "Run in api mode")
	flag.Parse()
	if *apiMode {
		//TODO: Add api mode
	}
	if *fileName == "" {
		fmt.Println("Please specify a file to extract")
		return
	}

	fileBytes, _ := os.ReadFile(*fileName)
	_, _, err := ProcessPyinstall(fileBytes)
	if err != nil {
		fmt.Println(err.Error())
	}
}