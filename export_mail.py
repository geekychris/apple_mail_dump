#!/usr/bin/env python3
"""
Export Apple Mail messages as NDJSON between two dates.
Directly queries the Apple Mail SQLite database for efficiency.
"""

import sqlite3
import json
import argparse
import os
import hashlib
import shutil
from pathlib import Path
from datetime import datetime
from email import policy
from email.parser import BytesParser
import sys


def find_envelope_database():
    """Locate the Apple Mail Envelope Index database."""
    mail_dir = Path.home() / "Library/Mail"
    
    # Find the latest Mail version directory (V10, V11, etc.)
    envelope_dbs = list(mail_dir.glob("V*/MailData/Envelope Index"))
    
    if not envelope_dbs:
        raise FileNotFoundError(
            f"Could not find Apple Mail database in {mail_dir}. "
            "Make sure Apple Mail is installed and has been run at least once."
        )
    
    # Return the most recent one (highest version number)
    return max(envelope_dbs, key=lambda p: int(p.parts[-3][1:]))


def get_message_file_path(message_id, mailbox_path):
    """
    Construct the file path for a message's .emlx file.
    Apple Mail stores messages in mailbox bundles.
    """
    mail_dir = Path.home() / "Library/Mail"
    
    # Try to find the message file
    # Messages are typically in V*/[Account]/[Mailbox].mbox/[ID].emlx
    for emlx_file in mail_dir.glob(f"V*/**/{message_id}.emlx"):
        return emlx_file
    
    return None


def parse_emlx_file(emlx_path, attachments_dir=None, message_id=None):
    """Parse an Apple Mail .emlx file and extract the email content and attachments."""
    try:
        with open(emlx_path, 'rb') as f:
            # First line is the message size, skip it
            f.readline()
            # Rest is the raw email
            raw_email = f.read()
            
        msg = BytesParser(policy=policy.default).parsebytes(raw_email)
        
        # Extract body text (prefer plain text, fall back to HTML)
        body_text = ""
        body_html = ""
        attachments = []
        
        if msg.is_multipart():
            for part in msg.walk():
                content_type = part.get_content_type()
                content_disposition = str(part.get("Content-Disposition", ""))
                
                # Extract text content
                if content_type == "text/plain" and "attachment" not in content_disposition:
                    try:
                        body_text = part.get_content()
                    except:
                        body_text = str(part.get_payload(decode=True), errors='replace')
                elif content_type == "text/html" and "attachment" not in content_disposition:
                    try:
                        body_html = part.get_content()
                    except:
                        body_html = str(part.get_payload(decode=True), errors='replace')
                
                # Extract attachments
                elif part.get_filename() or "attachment" in content_disposition:
                    filename = part.get_filename()
                    if filename and attachments_dir:
                        # Generate unique filename
                        file_hash = hashlib.md5(f"{message_id}_{filename}".encode()).hexdigest()[:8]
                        ext = Path(filename).suffix
                        unique_filename = f"{message_id}_{file_hash}{ext}"
                        
                        # Save attachment
                        attachment_path = attachments_dir / unique_filename
                        try:
                            with open(attachment_path, 'wb') as af:
                                af.write(part.get_payload(decode=True))
                            
                            attachments.append({
                                "filename": filename,
                                "saved_as": unique_filename,
                                "content_type": content_type,
                                "size": attachment_path.stat().st_size
                            })
                        except Exception as e:
                            attachments.append({
                                "filename": filename,
                                "error": str(e)
                            })
        else:
            # Not multipart - just get the content
            content_type = msg.get_content_type()
            if content_type == "text/plain":
                try:
                    body_text = msg.get_content()
                except:
                    body_text = str(msg.get_payload(decode=True), errors='replace')
            elif content_type == "text/html":
                try:
                    body_html = msg.get_content()
                except:
                    body_html = str(msg.get_payload(decode=True), errors='replace')
        
        # Use plain text if available, otherwise HTML
        body = body_text if body_text else body_html
        
        return {
            "body": body,
            "headers": dict(msg.items()),
            "attachments": attachments
        }
    except Exception as e:
        return {"body": "", "headers": {}, "attachments": [], "error": str(e)}


def export_messages(start_date, end_date, output_file=None, include_body=True, extract_attachments=True):
    """
    Export Apple Mail messages between two dates as NDJSON.
    
    Args:
        start_date: datetime object for start date
        end_date: datetime object for end date
        output_file: Path to output file (None for stdout)
        include_body: Whether to include message bodies (slower)
        extract_attachments: Whether to extract attachments to files
    """
    db_path = find_envelope_database()
    
    # Create attachments directory if extracting attachments
    attachments_dir = None
    if extract_attachments:
        attachments_dir = Path(output_file).parent / "attachments" if output_file else Path("attachments")
        attachments_dir.mkdir(exist_ok=True)
    
    # Convert datetime to Unix timestamps (seconds since 1970-01-01)
    start_timestamp = int(start_date.timestamp())
    end_timestamp = int(end_date.timestamp())
    
    # Open database in read-only mode
    conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    conn.row_factory = sqlite3.Row
    cursor = conn.cursor()
    
    # Query for messages in date range - join with subjects table to get actual subject text
    query = """
        SELECT 
            m.ROWID as message_id,
            s.subject as subject_text,
            m.sender,
            m.date_received,
            m.date_sent,
            m.remote_id,
            m.mailbox,
            a.address as sender_address,
            GROUP_CONCAT(DISTINCT ra.address) as recipients
        FROM messages m
        LEFT JOIN subjects s ON m.subject = s.ROWID
        LEFT JOIN addresses a ON m.sender = a.ROWID
        LEFT JOIN recipients r ON m.ROWID = r.message
        LEFT JOIN addresses ra ON r.address = ra.ROWID
        WHERE m.date_received BETWEEN ? AND ?
        GROUP BY m.ROWID
        ORDER BY m.date_received ASC
    """
    
    cursor.execute(query, (start_timestamp, end_timestamp))
    
    output = open(output_file, 'w') if output_file else sys.stdout
    
    try:
        count = 0
        total_attachments = 0
        last_date = None
        first_date = None
        
        for row in cursor:
            # Timestamps are already Unix timestamps
            date_received = row['date_received']
            date_sent = row['date_sent'] if row['date_sent'] else None
            
            # Track date range
            current_date = datetime.fromtimestamp(date_received)
            if first_date is None:
                first_date = current_date
            last_date = current_date
            
            record = {
                "message_id": row['message_id'],
                "subject": row['subject_text'],
                "sender": row['sender_address'],
                "recipients": row['recipients'].split(',') if row['recipients'] else [],
                "date_received": current_date.isoformat(),
                "date_sent": datetime.fromtimestamp(date_sent).isoformat() if date_sent else None,
                "remote_id": row['remote_id'],
                "mailbox": row['mailbox']
            }
            
            # Optionally include message body and attachments
            if include_body:
                emlx_path = get_message_file_path(row['message_id'], row['mailbox'])
                if emlx_path and emlx_path.exists():
                    parsed = parse_emlx_file(emlx_path, attachments_dir, row['message_id'])
                    record['body'] = parsed['body']
                    record['headers'] = parsed['headers']
                    record['attachments'] = parsed.get('attachments', [])
                    if 'error' in parsed:
                        record['parse_error'] = parsed['error']
                    
                    # Count attachments
                    num_attachments = len([a for a in record['attachments'] if 'error' not in a])
                    total_attachments += num_attachments
                else:
                    record['body'] = None
                    record['headers'] = {}
                    record['attachments'] = []
            
            output.write(json.dumps(record) + '\n')
            count += 1
            
            # Progress reporting every 100 messages
            if count % 100 == 0:
                print(f"Processed {count} messages (from {first_date.date()} to {last_date.date()}) | "
                      f"Attachments: {total_attachments}", file=sys.stderr)
        
        print(f"\n=== Export Complete ===", file=sys.stderr)
        print(f"Total messages exported: {count}", file=sys.stderr)
        print(f"Date range: {first_date.date() if first_date else 'N/A'} to {last_date.date() if last_date else 'N/A'}", file=sys.stderr)
        print(f"Total attachments extracted: {total_attachments}", file=sys.stderr)
        if attachments_dir:
            print(f"Attachments saved to: {attachments_dir}", file=sys.stderr)
        
    finally:
        if output_file:
            output.close()
        conn.close()


def main():
    parser = argparse.ArgumentParser(
        description="Export Apple Mail messages as NDJSON between two dates"
    )
    parser.add_argument(
        '--start-date',
        type=str,
        required=True,
        help='Start date in YYYY-MM-DD format'
    )
    parser.add_argument(
        '--end-date',
        type=str,
        required=True,
        help='End date in YYYY-MM-DD format'
    )
    parser.add_argument(
        '--output',
        type=str,
        help='Output file path (default: stdout)'
    )
    parser.add_argument(
        '--no-body',
        action='store_true',
        help='Exclude message bodies for faster export'
    )
    parser.add_argument(
        '--no-attachments',
        action='store_true',
        help='Do not extract attachments to files'
    )
    
    args = parser.parse_args()
    
    try:
        start_date = datetime.strptime(args.start_date, '%Y-%m-%d')
        end_date = datetime.strptime(args.end_date, '%Y-%m-%d')
        
        # Set end date to end of day
        end_date = end_date.replace(hour=23, minute=59, second=59)
        
        export_messages(
            start_date=start_date,
            end_date=end_date,
            output_file=args.output,
            include_body=not args.no_body,
            extract_attachments=not args.no_attachments
        )
        
    except ValueError as e:
        print(f"Error: Invalid date format. Use YYYY-MM-DD. {e}", file=sys.stderr)
        sys.exit(1)
    except FileNotFoundError as e:
        print(f"Error: {e}", file=sys.stderr)
        sys.exit(1)
    except Exception as e:
        print(f"Error: {e}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
