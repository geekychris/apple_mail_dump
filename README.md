# Apple Mail Export Script

A Python script to export Apple Mail messages as NDJSON (newline-delimited JSON) between specified dates. The script directly queries the Apple Mail SQLite database for efficient data extraction.

## Design

### Architecture

The script is organized into several key components:

1. **Database Discovery** (`find_envelope_database`)
   - Locates the Apple Mail Envelope Index database in `~/Library/Mail/V*/MailData/`
   - Automatically selects the most recent version if multiple exist

2. **Message Retrieval** (`export_messages`)
   - Opens the SQLite database in read-only mode
   - Queries messages within the specified date range using Unix timestamps
   - Joins multiple tables (messages, subjects, addresses, recipients) for complete data

3. **Email Parsing** (`parse_emlx_file`)
   - Reads Apple's `.emlx` file format (size header + raw RFC822 message)
   - Extracts plain text and HTML body content
   - Parses email headers
   - Extracts and saves attachments with unique filenames

4. **Output Format**
   - Each message is exported as a single JSON line (NDJSON format)
   - Includes metadata: subject, sender, recipients, dates, message ID
   - Optional: full body text, headers, and attachment information

### Data Flow

```
User Input (date range) 
  → Locate database 
  → Query SQLite for messages 
  → For each message:
      → Extract metadata from database
      → (Optional) Locate and parse .emlx file
      → (Optional) Extract attachments to disk
      → Write JSON record to output
```

## Requirements

- Python 3.6+
- macOS with Apple Mail installed
- Standard library only (no external dependencies)

## Installation

No installation required. The script uses only Python standard library modules.

Make the script executable:

```bash
chmod +x export_mail.py
```

## Usage

### Basic Examples

Export messages for a date range to stdout:

```bash
./export_mail.py --start-date 2024-01-01 --end-date 2024-01-31
```

Export to a file:

```bash
./export_mail.py --start-date 2024-01-01 --end-date 2024-01-31 --output emails.ndjson
```

Export only metadata (faster, no body content):

```bash
./export_mail.py --start-date 2024-01-01 --end-date 2024-01-31 --output emails.ndjson --no-body
```

Export without extracting attachments:

```bash
./export_mail.py --start-date 2024-01-01 --end-date 2024-01-31 --output emails.ndjson --no-attachments
```

### Command Line Options

- `--start-date YYYY-MM-DD` (required) - Start date for export
- `--end-date YYYY-MM-DD` (required) - End date for export (inclusive, through end of day)
- `--output FILE` (optional) - Output file path (defaults to stdout)
- `--no-body` (optional) - Skip message body parsing for faster export
- `--no-attachments` (optional) - Don't extract attachments to files

### Output Format

Each line in the output file is a JSON object with the following structure:

```json
{
  "message_id": 12345,
  "subject": "Email subject",
  "sender": "sender@example.com",
  "recipients": ["recipient1@example.com", "recipient2@example.com"],
  "date_received": "2024-01-15T10:30:45",
  "date_sent": "2024-01-15T10:30:42",
  "remote_id": "unique-message-id",
  "mailbox": 42,
  "body": "Email body text (if --no-body not used)",
  "headers": {"Header-Name": "value"},
  "attachments": [
    {
      "filename": "document.pdf",
      "saved_as": "12345_a1b2c3d4.pdf",
      "content_type": "application/pdf",
      "size": 102400
    }
  ]
}
```

Attachments are saved to an `attachments/` directory in the same location as the output file.

## macOS Permissions Setup

Apple Mail's database is protected by macOS privacy controls. You need to grant Full Disk Access to your terminal application.

### Granting Full Disk Access

1. **Open System Settings**
   - Click the Apple menu → System Settings (or System Preferences on older macOS)

2. **Navigate to Privacy & Security**
   - Click "Privacy & Security" in the sidebar
   - Scroll down and click "Full Disk Access"

3. **Add Your Terminal Application**
   - Click the lock icon (🔒) at the bottom and authenticate with your password
   - Click the "+" button to add an application
   - Navigate to and select your terminal application:
     - **Terminal.app**: `/Applications/Utilities/Terminal.app`
     - **iTerm2**: `/Applications/iTerm.app`
     - **Warp**: `/Applications/Warp.app`
     - Other terminal apps: find in `/Applications/`

4. **Enable the toggle** next to your terminal application

5. **Restart your terminal application** for changes to take effect

### Alternative: Using AppleScript

If you prefer not to grant Full Disk Access, you can use AppleScript to access Mail data through the Mail app API, though this is significantly slower and requires Mail.app to be running.

### Verifying Permissions

Test if permissions are correctly set by running:

```bash
ls ~/Library/Mail/V*/MailData/Envelope\ Index
```

If you see "Operation not permitted" or similar errors, Full Disk Access is not properly configured.

## Troubleshooting

### "Could not find Apple Mail database"
- Ensure Apple Mail has been opened at least once
- Check that Mail data exists in `~/Library/Mail/`

### "Operation not permitted" or "disk I/O error"
- You need to grant Full Disk Access (see Permissions Setup above)
- Make sure you've restarted your terminal after granting access

### Missing message bodies
- Some messages may not have corresponding `.emlx` files
- The script will set `body` to `null` for these messages

### Attachment extraction failures
- Individual attachment errors are recorded in the JSON output
- Check write permissions in the output directory

## Performance

- **Metadata only** (`--no-body`): Very fast, ~1000+ messages/second
- **Full export with bodies**: Moderate, ~50-200 messages/second depending on message size
- **With attachments**: Slower, depends on attachment sizes and disk I/O

Progress is reported every 100 messages to stderr.

## License

This script is provided as-is for personal use.
